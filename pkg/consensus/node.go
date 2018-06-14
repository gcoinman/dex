package consensus

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io/ioutil"
	"sync"
	"time"

	log "github.com/helinwang/log15"

	"github.com/dfinity/go-dfinity-crypto/bls"
)

// Node is a node in the consensus infrastructure.
//
// Nodes form a group randomly, the randomness comes from the random
// beacon.
type Node struct {
	addr  Addr
	cfg   Config
	sk    SK
	net   *Networking
	chain *Chain

	mu sync.Mutex
	// the memberships of different groups
	memberships    []membership
	notarizeChs    []chan *BlockProposal
	cancelNotarize func()
}

// NodeCredentials stores the credentials of the node.
type NodeCredentials struct {
	SK          SK
	Groups      []int
	GroupShares []SK
}

type membership struct {
	skShare bls.SecretKey
	groupID int
}

// Config is the consensus layer configuration.
type Config struct {
	BlockTime      time.Duration
	GroupSize      int
	GroupThreshold int
}

// NewNode creates a new node.
func NewNode(chain *Chain, sk SK, net *Networking, cfg Config) *Node {
	pk, err := sk.PK()
	if err != nil {
		panic(err)
	}

	addr := pk.Addr()
	n := &Node{
		addr:  addr,
		cfg:   cfg,
		sk:    sk,
		chain: chain,
		net:   net,
	}
	chain.n = n
	return n
}

// Chain returns node's block chain.
func (n *Node) Chain() *Chain {
	return n.chain
}

// Start starts the p2p network service.
func (n *Node) Start(myAddr, seedAddr string) {
	n.net.Start(myAddr, seedAddr)
}

// StartRound marks the start of the given round. It happens when the
// random beacon signature for the given round is received.
func (n *Node) _StartRound(round uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	log.Debug("start round", "round", round, "addr", n.addr)

	var ntCancelCtx context.Context
	_, bp, nt := n.chain.RandomBeacon.Committees(round)
	for _, m := range n.memberships {
		if m.groupID == bp {
			bp := n.chain.ProposeBlock(n.sk)
			go func() {
				log.Debug("proposing block", "addr", n.addr, "round", bp.Round, "hash", bp.Hash())
				n.net.recvBlockProposal(n.net.myself, bp)
			}()
		}

		if m.groupID == nt {
			if ntCancelCtx == nil {
				ntCancelCtx, n.cancelNotarize = context.WithCancel(context.Background())
			}

			notary := NewNotary(n.addr, n.sk.MustGet(), m.skShare, n.chain)
			inCh := make(chan *BlockProposal, 20)
			n.notarizeChs = append(n.notarizeChs, inCh)
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(n.cfg.BlockTime))
			go func() {
				onNotarize := func(s *NtShare) {
					go n.net.recvNtShare(s)
				}

				notary.Notarize(ctx, ntCancelCtx, inCh, onNotarize)
				cancel()
			}()
		}
	}
}

// EndRound marks the end of the given round. It happens when the
// block for the given round is received.
func (n *Node) EndRound(round uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()

	log.Debug("end round", "round", round, "addr", n.addr)

	n.notarizeChs = nil
	if n.cancelNotarize != nil {
		n.cancelNotarize()
	}

	rb, _, _ := n.chain.RandomBeacon.Committees(round)
	for _, m := range n.memberships {
		if m.groupID != rb {
			continue
		}
		// Current node is a member of the random
		// beacon committee, members collatively
		// produce the random beacon signature using
		// BLS threshold signature scheme. There are
		// multiple committees, which committee will
		// produce the next random beacon signature is
		// derived from the current random beacon
		// signature.
		keyShare := m.skShare
		go func() {
			history := n.chain.RandomBeacon.History()
			lastSigHash := SHA3(history[round].Sig)
			s := signRandBeaconShare(n.sk.MustGet(), keyShare, round+1, lastSigHash)
			n.net.recvRandBeaconSigShare(n.net.myself, s)
		}()
	}
}

// RecvBlockProposal tells the node that a valid block proposal of the
// current round is received.
func (n *Node) RecvBlockProposal(bp *BlockProposal) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for _, ch := range n.notarizeChs {
		ch <- bp
	}
}

func (n *Node) SendTxn(t []byte) {
	n.net.RecvTxn(t)
}

// MakeNode makes a new node with the given configurations.
func MakeNode(credentials NodeCredentials, net Network, cfg Config, genesis *Block, state State, txnPool TxnPool, u Updater) *Node {
	randSeed := Rand(SHA3([]byte("dex")))
	chain := NewChain(genesis, state, randSeed, cfg, txnPool, u)
	networking := NewNetworking(net, chain)
	node := NewNode(chain, credentials.SK, networking, cfg)
	for j := range credentials.Groups {
		share, err := credentials.GroupShares[j].Get()
		if err != nil {
			panic(err)
		}

		m := membership{groupID: credentials.Groups[j], skShare: share}
		node.memberships = append(node.memberships, m)
	}
	node.chain.RandomBeacon.n = node
	return node
}

// LoadCredential loads node credential from disk.
func LoadCredential(path string) (NodeCredentials, error) {
	var c NodeCredentials
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("open credential file failed: %v", err)
	}

	dec := gob.NewDecoder(bytes.NewReader(b))
	err = dec.Decode(&c)
	if err != nil {
		return c, fmt.Errorf("decode credential file failed: %v", err)
	}

	return c, nil
}
