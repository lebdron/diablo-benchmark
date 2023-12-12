package nsolana

import (
	"bytes"
	"context"
	"diablo-benchmark/core"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

type BlockchainClient struct {
	logger     core.Logger
	client     *rpc.Client
	ctx        context.Context
	commitment rpc.CommitmentType
	provider   parameterProvider
	preparer   transactionPreparer
	confirmer  transactionConfirmer
}

func newClient(logger core.Logger, client *rpc.Client, provider parameterProvider, preparer transactionPreparer, confirmer transactionConfirmer) *BlockchainClient {
	return &BlockchainClient{
		logger:     logger,
		client:     client,
		ctx:        context.Background(),
		commitment: rpc.CommitmentFinalized,
		provider:   provider,
		preparer:   preparer,
		confirmer:  confirmer,
	}
}

func (this *BlockchainClient) DecodePayload(encoded []byte) (interface{}, error) {
	buffer := bytes.NewBuffer(encoded)

	tx, err := decodeTransaction(buffer, this.provider)
	if err != nil {
		return nil, err
	}

	this.logger.Tracef("decode transaction %p", tx)

	err = this.preparer.prepare(tx)
	if err != nil {
		return nil, err
	}

	return tx, nil
}

func (this *BlockchainClient) TriggerInteraction(iact core.Interaction) error {
	tx := iact.Payload().(transaction)

	this.logger.Tracef("schedule transaction %p", tx)

	stx, err := tx.getTx()
	if err != nil {
		return err
	}

	hndl, err := this.confirmer.prepare(iact)
	if err != nil {
		return err
	}

	this.logger.Tracef("submit transaction %p", tx)

	iact.ReportSubmit()

	_, err = this.client.SendTransactionWithOpts(
		this.ctx,
		stx,
		rpc.TransactionOpts{
			SkipPreflight:       false,
			PreflightCommitment: this.commitment,
		})
	if err != nil {
		iact.ReportAbort()
		this.logger.Debugf("transaction aborted: %v", err)
		return err
	}

	return hndl.confirm()
}

type transactionPreparer interface {
	prepare(transaction) error
}

type nothingTransactionPreparer struct {
}

func newNothingTransactionPreparer() transactionPreparer {
	return &nothingTransactionPreparer{}
}

func (this *nothingTransactionPreparer) prepare(transaction) error {
	return nil
}

type transactionConfirmer interface {
	prepare(core.Interaction) (transactionConfirmerHandle, error)
}

type transactionConfirmerHandle interface {
	confirm() error
}

type subtxTransactionConfirmerHandle struct {
	logger core.Logger
	iact   core.Interaction
	sub    *ws.SignatureSubscription
	mwait  time.Duration
}

func (h *subtxTransactionConfirmerHandle) confirm() error {
	tx := h.iact.Payload().(transaction)

	defer h.sub.Unsubscribe()
	res, err := h.sub.RecvWithTimeout(h.mwait)
	if err != nil {
		return err
	}

	if res.Value.Err != nil {
		h.iact.ReportAbort()
		h.logger.Tracef("transaction %p failed (%v)", &res.Value.Err)
		return nil
	}

	h.iact.ReportCommit()
	h.logger.Tracef("transaction %p committed", tx)
	return nil
}

type subtxTransactionConfirmer struct {
	logger   core.Logger
	client   *rpc.Client
	wsClient *ws.Client
	ctx      context.Context
	mtime    time.Duration
}

func newSubtxTransactionConfirmer(logger core.Logger, client *rpc.Client, wsClient *ws.Client, ctx context.Context) *subtxTransactionConfirmer {
	return &subtxTransactionConfirmer{
		logger:   logger,
		client:   client,
		wsClient: wsClient,
		ctx:      context.Background(),
		mtime:    60 * time.Second,
	}
}

func (c *subtxTransactionConfirmer) prepare(iact core.Interaction) (transactionConfirmerHandle, error) {
	tx := iact.Payload().(transaction)

	stx, err := tx.getTx()
	if err != nil {
		return nil, err
	}

	sig := &stx.Signatures[0]
	sub, err := c.wsClient.SignatureSubscribe(*sig, rpc.CommitmentFinalized)
	if err != nil {
		return nil, err
	}

	return &subtxTransactionConfirmerHandle{
		logger: c.logger,
		iact:   iact,
		sub:    sub,
		mwait:  c.mtime,
	}, nil
}

type pollblkTransactionConfirmer struct {
	logger   core.Logger
	client   *rpc.Client
	wsClient *ws.Client
	ctx      context.Context
	err      error
	lock     sync.Mutex
	pendings map[solana.Signature]*pollblkTransactionConfirmerPending
}

type pollblkTransactionConfirmerPending struct {
	channel chan error
	iact    core.Interaction
}

func newPollblkTransactionConfirmer(logger core.Logger, client *rpc.Client, wsClient *ws.Client, ctx context.Context) *pollblkTransactionConfirmer {
	var this pollblkTransactionConfirmer

	this.logger = logger
	this.client = client
	this.wsClient = wsClient
	this.ctx = ctx
	this.err = nil
	this.pendings = make(map[solana.Signature]*pollblkTransactionConfirmerPending)

	go func(confirmer *pollblkTransactionConfirmer) {
		err := confirmer.run()
		confirmer.flushPendings(err)
	}(&this)

	return &this
}

func (c *pollblkTransactionConfirmer) prepare(iact core.Interaction) (transactionConfirmerHandle, error) {
	tx := iact.Payload().(transaction)

	stx, err := tx.getTx()
	if err != nil {
		return nil, err
	}

	hash := &stx.Signatures[0]

	channel := make(chan error)

	pending := &pollblkTransactionConfirmerPending{
		channel: channel,
		iact:    iact,
	}

	c.lock.Lock()

	var done bool
	if c.pendings == nil {
		done = true
	} else {
		c.pendings[*hash] = pending
		done = false
	}

	c.lock.Unlock()

	if done {
		close(channel)
		return nil, c.err
	}
	return pending, nil
}

func (p *pollblkTransactionConfirmerPending) confirm() error {
	return <-p.channel
}

func (c *pollblkTransactionConfirmer) reportHashes(hashes []solana.Signature) {
	pendings := make([]*pollblkTransactionConfirmerPending, 0, len(hashes))

	c.lock.Lock()

	for _, hash := range hashes {
		pending, ok := c.pendings[hash]
		if !ok {
			continue
		}

		delete(c.pendings, hash)

		pendings = append(pendings, pending)
	}

	c.lock.Unlock()

	for _, pending := range pendings {
		c.logger.Tracef("commit transaction %p",
			pending.iact.Payload())
		pending.iact.ReportCommit()
		pending.channel <- nil
		close(pending.channel)
	}
}

func (c *pollblkTransactionConfirmer) flushPendings(err error) {
	pendings := make([]*pollblkTransactionConfirmerPending, 0)

	c.lock.Lock()

	for _, pending := range c.pendings {
		pendings = append(pendings, pending)
	}

	c.pendings = nil
	c.err = err

	c.lock.Unlock()

	c.logger.Debugf("flush pendings with %v", err)

	for _, pending := range pendings {
		c.logger.Tracef("abort transaction %p",
			pending.iact.Payload())
		pending.iact.ReportAbort()
		pending.channel <- err
		close(pending.channel)
	}
}

func (c *pollblkTransactionConfirmer) processBlock(number uint64) error {
	var block *rpc.GetBlockResult
	var err error

	c.logger.Tracef("poll new block (number = %d)", number)

	includeRewards := false
	for {
		c.logger.Tracef("calling GetBlockWithOpts %v", number)
		block, err = c.client.GetBlockWithOpts(
			c.ctx,
			number,
			&rpc.GetBlockOpts{
				TransactionDetails: rpc.TransactionDetailsSignatures,
				Rewards:            &includeRewards,
				// should be safe as we request blocks for rooted slots
				Commitment: rpc.CommitmentConfirmed,
			})
		c.logger.Tracef("returned from GetBlockWithOpts %v", number)

		// rooted slot should be available
		var rpcerr *jsonrpc.RPCError
		if errors.As(err, &rpcerr) && rpcerr.Code == -32004 {
			c.logger.Tracef("GetBlockWithOpts error %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		return fmt.Errorf("GetBlockWithOpts failed: %w", err)
	}

	if len(block.Signatures) > 0 {
		c.reportHashes(block.Signatures)
	}

	return nil
}

func (c *pollblkTransactionConfirmer) run() error {
	subscription, err := c.wsClient.RootSubscribe()
	if err != nil {
		return fmt.Errorf("RootSubscribe failed: %w", err)
	}
	defer subscription.Unsubscribe()

	c.logger.Tracef("subscribe to new head events")

	// slot, err := c.client.GetFirstAvailableBlock(c.ctx)
	// if err != nil {
	// 	return fmt.Errorf("GetFirstAvailableBlock failed: %w", err)
	// }
	slot := uint64(0)
	subSlot := slot
	for {
		err := c.processBlock(slot)
		var rpcerr *jsonrpc.RPCError
		if errors.As(err, &rpcerr) && (rpcerr.Code == -32001 || rpcerr.Code == -32007) {
			if rpcerr.Code == -32001 {
				slot, err = strconv.ParseUint(rpcerr.Message[strings.LastIndex(rpcerr.Message, " ")+1:], 10, 64)
				if err != nil {
					return fmt.Errorf("failed to parse slot: %w", err)
				}
			} else if rpcerr.Code == -32007 {
				slot += 1
			}
			c.logger.Tracef("skipping to slot %v", slot)
			continue
		} else if errors.Is(err, rpc.ErrNotConfirmed) {
			c.logger.Tracef("processBlock %v failed with %v, skipping", slot, err)
		} else if err != nil {
			return fmt.Errorf("processBlock failed: %w", err)
		}
		c.logger.Tracef("processed slot %v", slot)
		slot++

		for subSlot < slot {
			event, err := subscription.Recv()
			if err != nil {
				return fmt.Errorf("Recv failed: %w", err)
			}
			if event == nil {
				return fmt.Errorf("received nil slot")
			}
			cand := uint64(*event)
			if cand > subSlot {
				subSlot = cand
			}
			c.logger.Tracef("received root slot %v, new slot %v", cand, subSlot)
		}
	}
}
