package nethereum

import (
	"bytes"
	"context"
	"diablo-benchmark/core"
	"math/big"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

type BlockchainClient struct {
	logger     core.Logger
	clients    []*ethclient.Client
	manager    nonceManager
	provider   parameterProvider
	preparer   transactionPreparer
	confirmer  transactionConfirmer
	rrEndpoint uint64
}

func newClient(logger core.Logger, clients []*ethclient.Client, manager nonceManager, provider parameterProvider, preparer transactionPreparer, confirmer transactionConfirmer) *BlockchainClient {
	return &BlockchainClient{
		logger:     logger,
		clients:    clients,
		manager:    manager,
		provider:   provider,
		preparer:   preparer,
		confirmer:  confirmer,
		rrEndpoint: 0,
	}
}

func (this *BlockchainClient) client() *ethclient.Client {
	endpoint := atomic.AddUint64(&this.rrEndpoint, 1)
	client := this.clients[endpoint%uint64(len(this.clients))]
	return client
}

func (this *BlockchainClient) DecodePayload(encoded []byte) (interface{}, error) {
	var buffer *bytes.Buffer = bytes.NewBuffer(encoded)
	var tx transaction
	var err error

	tx, err = decodeTransaction(buffer, this.manager, this.provider)
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
	var stx *types.Transaction
	var tx transaction
	var err error

	tx = iact.Payload().(transaction)

	this.logger.Tracef("schedule transaction %p", tx)

	stx, err = tx.getTx()
	if err != nil {
		return err
	}

	this.logger.Tracef("submit transaction %p", tx)

	iact.ReportSubmit()

	err = this.client().SendTransaction(context.Background(), stx)
	if err != nil {
		iact.ReportAbort()
		return err
	}

	return this.confirmer.confirm(iact)
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

type signatureTransactionPreparer struct {
	logger core.Logger
}

func newSignatureTransactionPreparer(logger core.Logger) transactionPreparer {
	return &signatureTransactionPreparer{
		logger: logger,
	}
}

func (this *signatureTransactionPreparer) prepare(tx transaction) error {
	var err error

	_, err = tx.getTx()
	if err != nil {
		return err
	}

	return nil
}

type transactionConfirmer interface {
	confirm(core.Interaction) error
}

type pollblkTransactionConfirmer struct {
	logger   core.Logger
	client   *ethclient.Client
	ctx      context.Context
	err      error
	lock     sync.Mutex
	pendings map[string]*pollblkTransactionConfirmerPending
	observer parameterObsrever
}

type pollblkTransactionConfirmerPending struct {
	channel chan<- error
	iact    core.Interaction
}

func newPollblkTransactionConfirmer(logger core.Logger, client *ethclient.Client, ctx context.Context, observer parameterObsrever) *pollblkTransactionConfirmer {
	var this pollblkTransactionConfirmer

	this.logger = logger
	this.client = client
	this.ctx = ctx
	this.err = nil
	this.pendings = make(map[string]*pollblkTransactionConfirmerPending)
	this.observer = observer

	go this.run()

	return &this
}

func (this *pollblkTransactionConfirmer) confirm(iact core.Interaction) error {
	var tx transaction = iact.Payload().(transaction)
	var pending *pollblkTransactionConfirmerPending
	var stx *types.Transaction
	var channel chan error
	var hash string
	var done bool
	var err error

	stx, err = tx.getTx()
	if err != nil {
		return err
	}

	hash = stx.Hash().String()

	channel = make(chan error)

	pending = &pollblkTransactionConfirmerPending{
		channel: channel,
		iact:    iact,
	}

	this.lock.Lock()

	if this.pendings == nil {
		done = true
	} else {
		this.pendings[hash] = pending
		done = false
	}

	this.lock.Unlock()

	if done {
		close(channel)
		return this.err
	} else {
		return <-channel
	}
}

func (this *pollblkTransactionConfirmer) reportHashes(hashes []string) {
	var pendings []*pollblkTransactionConfirmerPending
	var pending *pollblkTransactionConfirmerPending
	var hash string
	var ok bool

	pendings = make([]*pollblkTransactionConfirmerPending, 0, len(hashes))

	this.lock.Lock()

	for _, hash = range hashes {
		pending, ok = this.pendings[hash]
		if !ok {
			continue
		}

		delete(this.pendings, hash)

		pendings = append(pendings, pending)
	}

	this.lock.Unlock()

	for _, pending = range pendings {
		this.logger.Tracef("commit transaction %p",
			pending.iact.Payload())
		pending.iact.ReportCommit()
		pending.channel <- nil
		close(pending.channel)
	}
}

func (this *pollblkTransactionConfirmer) flushPendings(err error) {
	var pendings []*pollblkTransactionConfirmerPending
	var pending *pollblkTransactionConfirmerPending

	pendings = make([]*pollblkTransactionConfirmerPending, 0)

	this.lock.Lock()

	for _, pending = range this.pendings {
		pendings = append(pendings, pending)
	}

	this.pendings = nil
	this.err = err

	this.lock.Unlock()

	for _, pending = range pendings {
		this.logger.Tracef("abort transaction %p",
			pending.iact.Payload())
		pending.iact.ReportAbort()
		pending.channel <- err
		close(pending.channel)
	}
}

func (this *pollblkTransactionConfirmer) processBlock(number *big.Int) error {
	var stxs []*types.Transaction
	var stx *types.Transaction
	var block *types.Block
	var hashes []string
	var err error
	var i int

	this.logger.Tracef("poll new block (number = %d)", number)

	block, err = this.client.BlockByNumber(this.ctx, number)
	if err != nil {
		return err
	}

	go this.observer.updateParameters(block.Header())

	stxs = block.Transactions()
	hashes = make([]string, len(stxs))

	if len(stxs) == 0 {
		return nil
	}

	for i, stx = range stxs {
		hashes[i] = stx.Hash().String()
	}

	this.reportHashes(hashes)

	return nil
}

func (this *pollblkTransactionConfirmer) run() {
	var subcription ethereum.Subscription
	var events chan *types.Header
	var errors chan error
	var event *types.Header
	var err error

	events = make(chan *types.Header)
	errors = make(chan error)

	subcription, err = this.client.SubscribeNewHead(this.ctx, events)
	if err != nil {
		this.flushPendings(err)
		return
	}

	this.logger.Tracef("subscribe to new head events")

loop:
	for {
		select {
		case event = <-events:
			go func(number *big.Int) {
				if err := this.processBlock(number); err != nil {
					errors <- err
				}
			}(event.Number)
		case err = <-errors:
			break loop
		case err = <-subcription.Err():
			break loop
		case <-this.ctx.Done():
			err = this.ctx.Err()
			break loop
		}
	}

	subcription.Unsubscribe()

	close(events)
	close(errors)

	this.flushPendings(err)
}
