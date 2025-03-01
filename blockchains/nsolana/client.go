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

	params, err := this.provider.getParams(false)
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
			params, err := this.provider.getParams(true)
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
			logger.Debugf("pollblkTransactionConfirmer: processed block %v slot %v",
				result.result.Value.Block.Blockhash, result.result.Value.Slot)
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
	getParams(forceUpdate bool) (parameters, error)
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

func (p *directParameterProvider) getParams(forceUpdate bool) (parameters, error) {
	blockhash, err := p.client.GetLatestBlockhash(p.ctx, rpc.CommitmentFinalized)
	if err != nil {
		return parameters{}, err
	}

	return parameters{blockhash.Value.Blockhash}, nil
}

type cachedParameterProvider struct {
	params         parameters
	err            error
	lock           sync.RWMutex
	provider       parameterProvider
	updating       bool
	updateLock     sync.Mutex
	updateComplete chan struct{}
}

func (p *cachedParameterProvider) getNewBlockhash() (parameters, error) {
	p.lock.RLock()
	current := p.params
	p.lock.RUnlock()

	// Try up to a reasonable number of times to get a new blockhash
	for attempts := 0; attempts < 50; attempts++ {
		params, err := p.provider.getParams(true)
		if err != nil {
			return parameters{}, err
		}

		// If we got a different blockhash, return it
		if !current.blockhash.Equals(params.blockhash) {
			return params, nil
		}

		// Wait before trying again
		time.Sleep(default_ms_per_slot / 2)
	}

	// After several attempts, we still didn't get a new blockhash
	return parameters{}, fmt.Errorf("failed to get new blockhash after multiple attempts")
}

func newCachedParameterProvider(provider parameterProvider) (*cachedParameterProvider, error) {
	params, err := provider.getParams(false)
	if err != nil {
		return nil, fmt.Errorf("error during initialization: %w", err)
	}

	p := &cachedParameterProvider{
		params:         params,
		provider:       provider,
		updateComplete: make(chan struct{}),
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for range ticker.C {
			p.getParams(true)
		}
	}()

	return p, nil
}

func (p *cachedParameterProvider) getParams(forceUpdate bool) (parameters, error) {
	if !forceUpdate {
		// Fast path for regular calls - just return cached params
		p.lock.RLock()
		defer p.lock.RUnlock()
		return p.params, p.err
	}

	// For force update requests, we need to coordinate:

	// First try to get the update lock - only one goroutine should actually
	// perform the update
	p.updateLock.Lock()

	// Check if someone else is already updating
	if p.updating {
		// Someone else is updating, create a channel to wait on
		updateDone := p.updateComplete
		p.updateLock.Unlock()

		// Wait for the update to complete
		<-updateDone

		// Return the freshly updated params
		p.lock.RLock()
		defer p.lock.RUnlock()
		return p.params, p.err
	}

	// We're the one doing the update
	p.updating = true
	// Create a new channel for others to wait on
	p.updateComplete = make(chan struct{})
	p.updateLock.Unlock()

	// Get fresh parameters from the underlying provider
	params, err := p.getNewBlockhash()

	// Update our cached values
	p.lock.Lock()
	if err == nil {
		p.params = params
		p.err = nil
	} else {
		p.err = err
	}
	p.lock.Unlock()

	// Signal that the update is complete
	p.updateLock.Lock()
	p.updating = false
	close(p.updateComplete)
	p.updateLock.Unlock()

	return params, err
}
