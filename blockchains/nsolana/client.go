package nsolana

import (
	"bytes"
	"context"
	"diablo-benchmark/core"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
)

const max_processing_age = 150

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

	this.logger.Tracef("schedule transaction %p", tx)

	for i := 1; ; i++ {
		stx, err := tx.getTx(params.blockhash)
		if err != nil {
			return err
		}
		sig := stx.Signatures[0]

		hndl, err := this.confirmer.prepare(iact, sig, params.lastValidBlockHeight)
		if err != nil {
			return err
		}

		this.logger.Tracef("submit transaction %p, %v attempt %d", tx, sig, i)

		// will only record the first submission
		iact.ReportSubmit()

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

			this.confirmer.remove(sig)
		} else if err != nil {
			return fmt.Errorf("transaction %v failed: %w", sig, err)
		} else {
			result := hndl.confirm()
			if result.err != nil {
				return fmt.Errorf("transaction %v failed: %w", sig, result.err)
			}
			if !result.resend {
				return nil
			}
		}
		params, err = this.provider.getParams(params.blockhash)
		if err != nil {
			return err
		}
	}
}

type transactionConfirmer interface {
	prepare(core.Interaction, solana.Signature, uint64) (transactionConfirmerHandle, error)
	remove(solana.Signature)
}

type confirmResult struct {
	resend bool
	err    error
}

type transactionConfirmerHandle interface {
	confirm() confirmResult
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
	channel chan confirmResult
	iact    core.Interaction
	sig     solana.Signature
	height  uint64
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
				c.reportHashes(signatures, *result.result.Value.Block.BlockHeight)
			}
			logger.Debugf("pollblkTransactionConfirmer: processed block %v height %v",
				result.result.Value.Block.Blockhash, *result.result.Value.Block.BlockHeight)
		}
		c.flushPendings(err)
	}()

	return c
}

func (c *pollblkTransactionConfirmer) prepare(
	iact core.Interaction, sig solana.Signature, height uint64) (transactionConfirmerHandle, error) {
	channel := make(chan confirmResult, 1)

	pending := &pollblkTransactionConfirmerPending{
		channel: channel,
		iact:    iact,
		sig:     sig,
		height:  height,
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
		channel <- confirmResult{false, nil}
		close(channel)
		return pending, nil
	}

	// Regular path - add to pendings
	c.pendings[sig] = pending
	c.lock.Unlock()

	return pending, nil
}

func (c *pollblkTransactionConfirmer) remove(sig solana.Signature) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if c.pendings == nil {
		return
	}

	if pending, exists := c.pendings[sig]; exists {
		delete(c.pendings, sig)
		close(pending.channel)
	}
}

func (p *pollblkTransactionConfirmerPending) confirm() confirmResult {
	result, ok := <-p.channel
	if !ok {
		// Channel was closed, likely due to a new transaction being created
		return confirmResult{true, nil}
	}

	return result
}

func (c *pollblkTransactionConfirmer) reportHashes(hashes []solana.Signature, height uint64) {
	start := time.Now()
	count := len(hashes)

	pendings := make([]*pollblkTransactionConfirmerPending, 0, len(hashes))
	expired := make([]*pollblkTransactionConfirmerPending, 0)

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

	for sig, pending := range c.pendings {
		if pending.height < height {
			delete(c.pendings, sig)
			expired = append(expired, pending)
		}
	}

	c.lock.Unlock()

	for _, pending := range pendings {
		c.logger.Tracef("commit transaction %p, %v", pending.iact.Payload(), pending.sig)
		pending.iact.ReportCommit()
		select {
		case pending.channel <- confirmResult{false, nil}:
		default:
			c.logger.Warnf("transaction %p, %v: channel full or closed", pending.iact.Payload(), pending.sig)
		}
		close(pending.channel)
	}

	for _, pending := range expired {
		c.logger.Tracef("transaction %p, %v expired", pending.iact.Payload(), pending.sig)
		select {
		case pending.channel <- confirmResult{true, nil}: // resend
		default:
			c.logger.Warnf("transaction %p, %v: channel full or closed", pending.iact.Payload(), pending.sig)
		}
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
		pending.channel <- confirmResult{false, err}
		close(pending.channel)
	}
}

type parameters struct {
	blockhash            solana.Hash
	lastValidBlockHeight uint64
}

type parameterProvider interface {
	getParams(old solana.Hash) (parameters, error)
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

func (p *directParameterProvider) getParams(old solana.Hash) (parameters, error) {
	for {
		result, err := p.client.GetLatestBlockhash(p.ctx, rpc.CommitmentFinalized)
		if err != nil {
			return parameters{}, err
		}

		if old == result.Value.Blockhash {
			time.Sleep(default_ms_per_slot / 2)
			continue
		}

		return parameters{result.Value.Blockhash, result.Value.LastValidBlockHeight}, nil
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

func (p *cachedParameterProvider) getParams(old solana.Hash) (parameters, error) {
	// Fast path check with the cached value
	p.lock.RLock()
	last := p.last
	p.lock.RUnlock()

	if last.err != nil {
		return parameters{}, last.err
	}
	if last.result.Value.Block.Blockhash != old {
		return parameters{
			last.result.Value.Block.Blockhash, *last.result.Value.Block.BlockHeight + max_processing_age}, nil
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
		return parameters{}, result.err
	}
	return parameters{
		result.result.Value.Block.Blockhash, *result.result.Value.Block.BlockHeight + max_processing_age}, nil
}
