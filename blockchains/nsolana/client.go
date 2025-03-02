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
	logger     core.Logger
	client     *rpc.Client
	subscriber *blockSubscriber
	ctx        context.Context
	commitment rpc.CommitmentType
	provider   parameterProvider
	confirmer  transactionConfirmer
}

func newClient(
	logger core.Logger,
	client *rpc.Client,
	subscriber *blockSubscriber,
	provider parameterProvider,
	confirmer transactionConfirmer) *BlockchainClient {
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			result, err := client.GetLatestBlockhash(context.Background(), rpc.CommitmentFinalized)
			if err != nil {
				logger.Errorf("GetLatestBlockhash failed: %v", err)
				continue
			}
			logger.Debugf("Latest blockhash: %v last valid height %d",
				result.Value.Blockhash, result.Value.LastValidBlockHeight)
		}
	}()

	return &BlockchainClient{
		logger:     logger,
		client:     client,
		subscriber: subscriber,
		ctx:        context.Background(),
		commitment: rpc.CommitmentFinalized,
		provider:   provider,
		confirmer:  confirmer,
	}
}

func (this *BlockchainClient) DecodePayload(encoded []byte) (interface{}, error) {
	buffer := bytes.NewBuffer(encoded)

	tx, err := decodeTransaction(buffer)
	if err != nil {
		return nil, err
	}

	this.logger.Tracef("decode transaction %p", tx)

	return tx, nil
}

func (this *BlockchainClient) TriggerInteraction(iact core.Interaction) error {
	tx := iact.Payload().(transaction)

	params, err := this.provider.getParams(solana.Hash{})
	if err != nil {
		return err
	}

	stx, err := tx.getTx(params)
	if err != nil {
		return err
	}
	sig := stx.Signatures[0]

	this.logger.Tracef("schedule transaction %p, %v", tx, sig)

	hndl, err := this.confirmer.prepare(iact, sig)
	if err != nil {
		return err
	}

	this.logger.Tracef("submit transaction %p, %v", tx, sig)

	iact.ReportSubmit()

	for {
		_, err = this.client.SendTransactionWithOpts(
			this.ctx,
			stx,
			rpc.TransactionOpts{
				SkipPreflight:       false,
				PreflightCommitment: this.commitment,
			})

		if rpcerr, ok := err.(*jsonrpc.RPCError); ok &&
			rpcerr.Code == -32002 &&
			strings.Contains(rpcerr.Message, "Blockhash not found") {
			params, err = this.provider.getParams(params)
			if err != nil {
				return err
			}
			stx, err = tx.getTx(params)
			if err != nil {
				return err
			}
			continue
		}
		break
	}

	if err != nil {
		iact.ReportAbort()
		this.logger.Debugf("transaction %v aborted: %v", sig, err)
		return err
	}

	return hndl.confirm()
}

type transactionConfirmer interface {
	prepare(core.Interaction, solana.Signature) (transactionConfirmerHandle, error)
}

type transactionConfirmerHandle interface {
	confirm() error
}

type subtxTransactionConfirmerHandle struct {
	logger core.Logger
	iact   core.Interaction
	sig    solana.Signature
	sub    *ws.SignatureSubscription
	mwait  time.Duration
}

func (h *subtxTransactionConfirmerHandle) confirm() error {
	defer h.sub.Unsubscribe()
	res, err := h.sub.RecvWithTimeout(h.mwait)
	if err != nil {
		return err
	}

	if res.Value.Err != nil {
		h.iact.ReportAbort()
		h.logger.Tracef("transaction %p, %v failed (%v)", h.iact.Payload(), h.sig, &res.Value.Err)
		return nil
	}

	h.iact.ReportCommit()
	h.logger.Tracef("transaction %p, %v committed", h.iact.Payload(), h.sig)
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

func (c *subtxTransactionConfirmer) prepare(
	iact core.Interaction, sig solana.Signature) (transactionConfirmerHandle, error) {
	sub, err := c.wsClient.SignatureSubscribe(sig, rpc.CommitmentFinalized)
	if err != nil {
		return nil, err
	}

	return &subtxTransactionConfirmerHandle{
		logger: c.logger,
		iact:   iact,
		sig:    sig,
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
	sig     solana.Signature
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
			signatures := result.result.Value.Block.Signatures
			if len(signatures) > 0 {
				c.reportHashes(signatures)
			}
			logger.Debugf("pollblkTransactionConfirmer: processed block %v height %v",
				result.result.Value.Block.Blockhash, *result.result.Value.Block.BlockHeight)
		}
		c.flushPendings(err)
	}()

	return c
}

func (c *pollblkTransactionConfirmer) prepare(
	iact core.Interaction, sig solana.Signature) (transactionConfirmerHandle, error) {
	channel := make(chan error, 1)

	pending := &pollblkTransactionConfirmerPending{
		channel: channel,
		iact:    iact,
		sig:     sig,
	}

	c.lock.Lock()

	// Early return checks
	if c.pendings == nil {
		c.lock.Unlock()
		close(channel)
		return nil, c.err
	}

	if _, ok := c.committed[sig]; ok {
		delete(c.committed, sig)
		c.lock.Unlock()
		iact.ReportCommit()
		channel <- nil
		close(channel)
		return pending, nil
	}

	// Regular path - add to pendings
	c.pendings[sig] = pending
	c.lock.Unlock()

	return pending, nil
}

func (p *pollblkTransactionConfirmerPending) confirm() error {
	return <-p.channel
}

func (c *pollblkTransactionConfirmer) reportHashes(hashes []solana.Signature) {
	start := time.Now()
	count := len(hashes)

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
		c.logger.Tracef("commit transaction %p, %v", pending.iact.Payload(), pending.sig)
		pending.iact.ReportCommit()
		pending.channel <- nil
		close(pending.channel)
	}

	elapsed := time.Since(start)
	c.logger.Debugf("Processed %d signatures in %v (%.2f per second)",
		count, elapsed, float64(count)/elapsed.Seconds())
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
		c.logger.Tracef("abort transaction %p, %v", pending.iact.Payload(), pending.sig)
		pending.iact.ReportAbort()
		pending.channel <- err
		close(pending.channel)
	}
}

type parameterProvider interface {
	getParams(old solana.Hash) (solana.Hash, error)
}

type directParameterProvider struct {
	client *rpc.Client
	ctx    context.Context
}

func newDirectParameterProvider(client *rpc.Client, ctx context.Context) *directParameterProvider {
	return &directParameterProvider{
		client: client,
		ctx:    ctx,
	}
}

func (p *directParameterProvider) getParams(old solana.Hash) (solana.Hash, error) {
	for {
		result, err := p.client.GetLatestBlockhash(p.ctx, rpc.CommitmentFinalized)
		if err != nil {
			return solana.Hash{}, err
		}

		if old == result.Value.Blockhash {
			time.Sleep(default_ms_per_slot / 2)
			continue
		}

		return result.Value.Blockhash, nil
	}
}

type waiter struct {
	blockhash solana.Hash
	notify    chan<- blockResult
}

type cachedParameterProvider struct {
	logger  core.Logger
	last    blockResult
	lock    sync.RWMutex
	updates <-chan blockResult
	waiters []waiter
}

func newCachedParameterProvider(logger core.Logger, blocks <-chan blockResult) (*cachedParameterProvider, error) {
	result := <-blocks
	if result.err != nil {
		return nil, result.err
	}

	p := &cachedParameterProvider{
		logger:  logger,
		last:    result,
		updates: blocks,
		waiters: make([]waiter, 0),
	}

	go func() {
		for update := range p.updates {
			p.lock.Lock()
			p.last = update

			// Notify any waiters that might be interested in this block
			numBefore := len(p.waiters)
			newWaiters := make([]waiter, 0, len(p.waiters))
			for _, w := range p.waiters {
				if update.result.Value.Block.Blockhash != w.blockhash {
					// This waiter is looking for a different blockhash than this one
					// Notify them of the new block
					select {
					case w.notify <- update:
						// Successfully notified, don't keep in waiters list
					default:
						// Couldn't notify, keep in list
						newWaiters = append(newWaiters, w)
					}
				} else {
					// This block has the same hash they're comparing against
					// Keep them in the waiters list
					newWaiters = append(newWaiters, w)
				}
			}
			p.waiters = newWaiters
			numAfter := len(p.waiters)
			p.lock.Unlock()
			p.logger.Debugf("cachedParameterProvider: processed block %v height %v waiters %v -> %v",
				update.result.Value.Block.Blockhash, *update.result.Value.Block.BlockHeight, numBefore, numAfter)
		}
	}()

	return p, nil
}

func (p *cachedParameterProvider) getParams(old solana.Hash) (solana.Hash, error) {
	// Fast path check with the cached value
	p.lock.RLock()
	last := p.last
	p.lock.RUnlock()

	if last.err != nil {
		return solana.Hash{}, last.err
	}
	if last.result.Value.Block.Blockhash != old {
		return last.result.Value.Block.Blockhash, nil
	}

	// Set up a channel just for this caller
	newBlockCh := make(chan blockResult, 1)

	// Register our interest in new blocks
	p.lock.Lock()
	p.waiters = append(p.waiters, waiter{
		blockhash: old,
		notify:    newBlockCh,
	})
	p.lock.Unlock()

	// Wait for notification of a matching block
	result := <-newBlockCh
	if result.err != nil {
		return solana.Hash{}, result.err
	}
	return result.result.Value.Block.Blockhash, nil
}
