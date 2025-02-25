package nsolana

import (
	"bytes"
	"context"
	"diablo-benchmark/core"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

type BlockchainClient struct {
	logger      core.Logger
	client      *rpc.Client
	blockClient *blockClient
	ctx         context.Context
	commitment  rpc.CommitmentType
	provider    parameterProvider
	preparer    transactionPreparer
	confirmer   transactionConfirmer
}

func newClient(logger core.Logger, client *rpc.Client, blockClient *blockClient, provider parameterProvider, preparer transactionPreparer, confirmer transactionConfirmer) *BlockchainClient {
	return &BlockchainClient{
		logger:      logger,
		client:      client,
		blockClient: blockClient,
		ctx:         context.Background(),
		commitment:  rpc.CommitmentFinalized,
		provider:    provider,
		preparer:    preparer,
		confirmer:   confirmer,
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

	stx, err := tx.getTx()
	if err != nil {
		return err
	}

	this.logger.Tracef("schedule transaction %p, %v", tx, stx.Signatures[0])

	hndl, err := this.confirmer.prepare(iact)
	if err != nil {
		return err
	}

	this.logger.Tracef("submit transaction %p, %v", tx, stx.Signatures[0])

	iact.ReportSubmit()

	_, err = this.client.SendTransactionWithOpts(
		this.ctx,
		stx,
		rpc.TransactionOpts{
			SkipPreflight:       false,
			PreflightCommitment: this.commitment,
		})
	isBlockhashNotFound := func(err error) bool {
		rpcerr, ok := err.(*jsonrpc.RPCError)
		return ok && rpcerr.Code == -32002 && strings.Contains(rpcerr.Message, "Blockhash not found")
	}
	if err != nil && !isBlockhashNotFound(err) {
		iact.ReportAbort()
		this.logger.Debugf("transaction %v aborted: %v", stx.Signatures[0], err)
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

	stx, _ := tx.getTx()

	if res.Value.Err != nil {
		h.iact.ReportAbort()
		h.logger.Tracef("transaction %p, %v failed (%v)", stx.Signatures[0], &res.Value.Err)
		return nil
	}

	h.iact.ReportCommit()
	h.logger.Tracef("transaction %p, %v committed", stx.Signatures[0], tx)
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
	logger    core.Logger
	blocks    <-chan blockResult
	err       error
	lock      sync.Mutex
	pendings  map[solana.Signature]*pollblkTransactionConfirmerPending
	committed map[solana.Signature]struct{}
}

type pollblkTransactionConfirmerPending struct {
	channel chan error
	iact    core.Interaction
}

func newPollblkTransactionConfirmer(logger core.Logger, blocks <-chan blockResult) *pollblkTransactionConfirmer {
	c := &pollblkTransactionConfirmer{
		logger:    logger,
		blocks:    blocks,
		pendings:  make(map[solana.Signature]*pollblkTransactionConfirmerPending),
		committed: make(map[solana.Signature]struct{}),
	}

	go func() {
		var err error
		for result := range blocks {
			if result.err != nil {
				err = result.err
				break
			}
			if len(result.block.Signatures) > 0 {
				c.reportHashes(result.block.Signatures)
			}
		}
		c.flushPendings(err)
	}()

	return c
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

	done := false
	if c.pendings == nil {
		done = true
	} else if _, ok := c.committed[*hash]; ok {
		delete(c.committed, *hash)
		iact.ReportCommit()
		channel <- nil
		close(channel)
	} else {
		c.pendings[*hash] = pending
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
			c.committed[hash] = struct{}{}
			continue
		}

		delete(c.pendings, hash)

		pendings = append(pendings, pending)
	}

	c.lock.Unlock()

	for _, pending := range pendings {
		stx, _ := pending.iact.Payload().(transaction).getTx()
		c.logger.Tracef("commit transaction %p, %v",
			pending.iact.Payload(), stx.Signatures[0])
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
		stx, _ := pending.iact.Payload().(transaction).getTx()
		c.logger.Tracef("abort transaction %p, %v",
			pending.iact.Payload(), stx.Signatures[0])
		pending.iact.ReportAbort()
		pending.channel <- err
		close(pending.channel)
	}
}
