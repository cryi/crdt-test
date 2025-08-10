package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ipfslite "github.com/hsanjuan/ipfs-lite"
	"github.com/ipfs/boxo/ipns"
	ds "github.com/ipfs/go-datastore"
	badger4 "github.com/ipfs/go-ds-badger4"
	crdt "github.com/ipfs/go-ds-crdt"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	dualdht "github.com/libp2p/go-libp2p-kad-dht/dual"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	record "github.com/libp2p/go-libp2p-record"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	multiaddr "github.com/multiformats/go-multiaddr"
)

const (
	crdtStoreId = `crdt-store`
	netTopic    = `crdt-test`
)

type node struct {
	Host      host.Host
	relay     *relay.Relay
	dht       *dualdht.DHT
	pubSub    *pubsub.PubSub
	dataStore *badger4.Datastore
	ipfsPeer  *ipfslite.Peer
	netTopic  *pubsub.Topic
}

func (node *node) Close() error {
	var err error
	if node.Host != nil {
		err = errors.Join(err, node.Host.Close())
	}
	if node.dht != nil {
		err = errors.Join(err, node.dht.Close())
	}

	if node.netTopic != nil {
		err = errors.Join(err, node.netTopic.Close())
	}

	if node.dataStore != nil {
		err = errors.Join(err, node.dataStore.Close())
	}
	return err
}

func newDHT(ctx context.Context, h host.Host) (*dualdht.DHT, error) {
	dhtOpts := []dualdht.Option{
		dualdht.DHTOption(dht.NamespacedValidator("pk", record.PublicKeyValidator{})),
		dualdht.DHTOption(dht.NamespacedValidator("ipns", ipns.Validator{KeyBook: h.Peerstore()})),
		dualdht.DHTOption(dht.Concurrency(10)),
		dualdht.DHTOption(dht.Mode(dht.ModeServer)),
	}

	dht, err := dualdht.New(ctx, h, dhtOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create DHT: %w", err)
	}

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			dht.Bootstrap(ctx)
		}
	}()

	return dht, nil
}

func main() {
	var listen, connect string
	flag.StringVar(&listen, "listen", "/ip4/0.0.0.0/tcp/9999", "multiaddress to listen on")
	flag.StringVar(&connect, "connect", "", "multiaddress to connect to")
	flag.Parse()

	listenAddr, err := multiaddr.NewMultiaddr(listen)
	if err != nil {
		panic(fmt.Sprintf("invalid listen address: %v\n", err))
	}

	var connectPeer *peer.AddrInfo
	if connect != "" {
		connectAddr, err := multiaddr.NewMultiaddr(connect)
		if err != nil {
			panic("invalid connect flag")
		}
		connectPeer, err = peer.AddrInfoFromP2pAddr(connectAddr)
		if err != nil {
			panic("invalid connect flag")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	secret, _ := base64.StdEncoding.DecodeString("4EAKNnP7P20cfTdNMO927WpapoA3O4foBCq1caqfXas=")
	node := &node{}

	priv, _, err := crypto.GenerateKeyPair(crypto.Ed25519, 1)
	if err != nil {
		panic(err.Error())
	}
	node.Host, node.dht, err = ipfslite.SetupLibp2p(ctx, priv, secret, []multiaddr.Multiaddr{listenAddr}, nil, ipfslite.Libp2pOptionsExtra...)
	if err != nil {
		panic(err.Error())
	}

	node.Host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(net network.Network, conn network.Conn) {
			slog.Info("connected to peer", "peer_id", conn.RemotePeer().String(), "local_addr", conn.LocalMultiaddr().String(), "remote_addr", conn.RemoteMultiaddr().String())
		},
		DisconnectedF: func(net network.Network, conn network.Conn) {
			slog.Info("disconnected from peer", "peer_id", conn.RemotePeer().String(), "local_addr", conn.LocalMultiaddr().String(), "remote_addr", conn.RemoteMultiaddr().String())
		},
	})

	fmt.Println("============================================")
	fmt.Println("Host ID:", node.Host.ID())
	fmt.Println("============================================")
	time.Sleep(2 * time.Second)

	node.pubSub, err = pubsub.NewGossipSub(ctx, node.Host)
	if err != nil {
		panic(err.Error())
	}

	node.netTopic, err = node.pubSub.Join(netTopic)
	if err != nil {
		panic(err.Error())
	}

	netSubs, err := node.netTopic.Subscribe()
	if err != nil {
		panic(err.Error())
	}

	go func() {
		for {
			msg, err := netSubs.Next(ctx)
			if err != nil {
				fmt.Println(err)
				break
			}
			node.Host.ConnManager().TagPeer(msg.ReceivedFrom, "keep", 100)
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
				node.netTopic.Publish(ctx, []byte("alive"))
				time.Sleep(20 * time.Second)
			}
		}
	}()

	node.dataStore, err = badger4.NewDatastore("crdt-store", &badger4.DefaultOptions)
	if err != nil {
		return
	}

	node.ipfsPeer, err = ipfslite.New(ctx, node.dataStore, nil, node.Host, node.dht, nil)
	if err != nil {
		return
	}

	if connectPeer != nil {
		node.ipfsPeer.Bootstrap([]peer.AddrInfo{*connectPeer})
	}

	pubsubBroadcaster, err := crdt.NewPubSubBroadcaster(ctx, node.pubSub, crdtStoreId)
	if err != nil {
		panic(err.Error())
	}

	opts := crdt.DefaultOptions()
	opts.PutHook = func(key ds.Key, value []byte) {
		if !strings.Contains(key.String(), node.Host.ID().String()) {
			slog.Info("CRDT Put", "key", key, "value", string(value))
		}
	}
	store, err := crdt.New(node.dataStore, ds.NewKey(crdtStoreId), node.ipfsPeer, pubsubBroadcaster, opts)
	if err != nil {
		panic(err.Error())
	}

	closeChannel := make(chan os.Signal, 1)
	signal.Notify(closeChannel, os.Interrupt, syscall.SIGTERM)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}

			store.Put(ctx, ds.NewKey(node.Host.ID().String()+"/time"), []byte(time.Now().String()))
		}
	}()

	<-closeChannel
	slog.Info("shutting down...")
	node.Close()
	cancel()
}
