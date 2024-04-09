package naptos

import (
	"bytes"
	"context"
	"diablo-benchmark/core"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	aptosclient "github.com/portto/aptos-go-sdk/client"
	aptosmodels "github.com/portto/aptos-go-sdk/models"
)

const (
	VmStatusExecuted = "Executed successfully"
	txTypeUser       = "user_transaction"
)

type BlockchainClient struct {
	logger    core.Logger
	client    aptosclient.AptosClient
	ctx       context.Context
	preparer  transactionPreparer
	confirmer transactionConfirmer
}

func newClient(logger core.Logger, client aptosclient.AptosClient, preparer transactionPreparer, confirmer transactionConfirmer) *BlockchainClient {
	return &BlockchainClient{
		logger:    logger,
		client:    client,
		ctx:       context.Background(),
		preparer:  preparer,
		confirmer: confirmer,
	}
}

func (c *BlockchainClient) DecodePayload(encoded []byte) (interface{}, error) {
	var buffer *bytes.Buffer = bytes.NewBuffer(encoded)

	tx, err := decodeTransaction(buffer)
	if err != nil {
		return nil, err
	}

	c.logger.Tracef("decode transaction %s", tx.getName())

	err = c.preparer.prepare(tx)
	if err != nil {
		c.logger.Debugf("error decoding transaction %v", err)
		return nil, err
	}

	return tx, nil
}

func (c *BlockchainClient) TriggerInteraction(iact core.Interaction) error {
	tx := iact.Payload().(transaction)

	c.logger.Tracef("schedule transaction %s", tx.getName())

	stx, err := tx.getSigned()
	if err != nil {
		return err
	}

	c.confirmer.prepare(iact)

	c.logger.Tracef("submit transaction %s", tx.getName())

	iact.ReportSubmit()

	_, err = c.client.SubmitTransaction(c.ctx, *stx)
	if err != nil {
		return fmt.Errorf("transaction %s failed (%s)", tx.getName(),
			err.Error())
	}

	return c.confirmer.confirm(iact)
}

type transactionPreparer interface {
	prepare(transaction) error
}

type nothingTransactionPreparer struct {
}

func newNothingTransactionPreparer() transactionPreparer {
	return &nothingTransactionPreparer{}
}

func (p *nothingTransactionPreparer) prepare(transaction) error {
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

func (p *signatureTransactionPreparer) prepare(tx transaction) error {
	var err error

	p.logger.Tracef("sign transaction %s", tx.getName())

	_, err = tx.getSigned()
	if err != nil {
		return err
	}

	return nil
}

type transactionConfirmer interface {
	prepare(core.Interaction)
	confirm(core.Interaction) error
}

type polltxTransactionConfirmer struct {
	logger core.Logger
	client aptosclient.AptosClient
	ctx    context.Context
	mwait  time.Duration
}

func newPolltxTransactionConfirmer(logger core.Logger, client aptosclient.AptosClient) *polltxTransactionConfirmer {
	return &polltxTransactionConfirmer{
		logger: logger,
		client: client,
		ctx:    context.Background(),
		mwait:  30 * time.Second,
	}
}

func (c *polltxTransactionConfirmer) prepare(core.Interaction) {
}

// InvalidTransactionError is error for get a transaction with unexpected details (e.g. vm status is failure)
type InvalidTransactionError struct {
	Transaction *aptosclient.TransactionResp
	Msg         string
}

// Error implements error interface
func (e *InvalidTransactionError) Error() string {
	return e.Msg
}

func WaitForTransaction(client aptosclient.AptosClient, ctx context.Context, tx *aptosmodels.UserTransaction, timeout time.Duration) (*aptosclient.TransactionResp, error) {
	seq := tx.SequenceNumber
	address := tx.Sender.ToHex()
	hash, err := TransactionHash(tx)
	if err != nil {
		return nil, err
	}
	expirationTimeSec := tx.ExpirationTimestampSecs
	rspHeader := new(aptosclient.ResponseHeader)
	step := time.Millisecond * 500
	start := time.Now()
	for {
		if time.Since(start) > timeout {
			break
		}
		txns, err := client.GetAccountTransactions(ctx, address, seq, 1, rspHeader)
		if err != nil {
			return nil, err
		}
		if len(txns) == 1 {
			txn := txns[0]
			if txn.Hash != hash {
				return nil, &InvalidTransactionError{
					Transaction: &txn,
					Msg: fmt.Sprintf(
						"transaction hash does not match, given %#v, but got %#v",
						hash, txn.Hash),
				}
			}
			return &txn, nil
		}
		if expirationTimeSec*1_000_000 <= rspHeader.AptosLedgerTimestampusec {
			return nil, errors.New("transaction expired")
		}
		time.Sleep(step)
	}
	return nil, fmt.Errorf("transaction not found within timeout period: %v", timeout)
}

func (c *polltxTransactionConfirmer) confirm(iact core.Interaction) error {
	tx := iact.Payload().(transaction)

	stx, err := tx.getSigned()
	if err != nil {
		return err
	}

	state, err := WaitForTransaction(c.client, c.ctx, stx, c.mwait)
	if err != nil {
		return err
	}

	if state.VmStatus != VmStatusExecuted {
		iact.ReportAbort()
		c.logger.Tracef("transaction %s failed (%s)", tx.getName(),
			state.VmStatus)
		return nil
	}

	iact.ReportCommit()
	c.logger.Tracef("transaction %s committed", tx.getName())
	return nil
}

type pollblkTransactionConfirmer struct {
	logger    core.Logger
	client    aptosclient.AptosClient
	ctx       context.Context
	err       error
	lock      sync.Mutex
	pendings  map[pollblkTransactionConfirmerKey]*pollblkTransactionConfirmerPending
	committed map[pollblkTransactionConfirmerKey]struct{}
}

type pollblkTransactionConfirmerKey struct {
	sender   aptosmodels.AccountAddress
	sequence uint64
}

type pollblkTransactionConfirmerPending struct {
	channel chan error
	iact    core.Interaction
}

func newPollblkTransactionConfirmer(logger core.Logger, client aptosclient.AptosClient) *pollblkTransactionConfirmer {
	var this pollblkTransactionConfirmer

	this.logger = logger
	this.client = client
	this.ctx = context.Background()
	this.err = nil
	this.pendings = make(map[pollblkTransactionConfirmerKey]*pollblkTransactionConfirmerPending)
	this.committed = make(map[pollblkTransactionConfirmerKey]struct{})

	go this.run()

	return &this
}

func (c *pollblkTransactionConfirmer) prepare(iact core.Interaction) {
	var tx transaction = iact.Payload().(transaction)

	stx, _ := tx.getSigned()

	channel := make(chan error)

	key := pollblkTransactionConfirmerKey{
		sender:   stx.Sender,
		sequence: stx.SequenceNumber,
	}

	value := &pollblkTransactionConfirmerPending{
		channel: channel,
		iact:    iact,
	}

	c.lock.Lock()

	if _, ok := c.committed[key]; ok {
		delete(c.committed, key)
		iact.ReportCommit()
		channel <- nil
		close(channel)
	}

	if c.pendings != nil {
		c.pendings[key] = value
	}

	c.lock.Unlock()
}

func (c *pollblkTransactionConfirmer) confirm(iact core.Interaction) error {
	var tx transaction = iact.Payload().(transaction)

	stx, _ := tx.getSigned()

	key := pollblkTransactionConfirmerKey{
		sender:   stx.Sender,
		sequence: stx.SequenceNumber,
	}

	c.lock.Lock()

	var value *pollblkTransactionConfirmerPending
	if c.pendings != nil {
		value = c.pendings[key]
	}

	c.lock.Unlock()

	if value == nil {
		return c.err
	} else {
		return <-value.channel
	}
}

func (c *pollblkTransactionConfirmer) parseTransaction(tx *aptosclient.TransactionResp) {
	account, err := aptosmodels.HexToAccountAddress(tx.Sender)
	if err != nil {
		c.logger.Errorf("parse account address: %s", err.Error())
		return
	}

	sequenceNumber, err := strconv.ParseUint(tx.SequenceNumber, 10, 64)
	if err != nil {
		c.logger.Errorf("parse sequence number: %s", err.Error())
		return
	}
	key := pollblkTransactionConfirmerKey{
		sender:   account,
		sequence: sequenceNumber,
	}

	c.lock.Lock()

	pending, ok := c.pendings[key]
	if ok {
		delete(c.pendings, key)
	} else {
		c.committed[key] = struct{}{}
	}

	c.lock.Unlock()

	if !ok {
		return
	}

	pending.iact.ReportCommit()

	c.logger.Tracef("transaction %s committed",
		pending.iact.Payload().(transaction).getName())

	pending.channel <- nil

	close(pending.channel)
}

func (c *pollblkTransactionConfirmer) run() {
	meta, err := c.client.LedgerInformation(c.ctx)
	if err != nil {
		c.logger.Errorf("get meta: %s", err.Error())
		return
	}

	v, err := strconv.ParseUint(meta.LedgerVersion, 10, 64)
	if err != nil {
		c.logger.Errorf("parse ledger version: %s", err.Error())
		return
	}

	for {
		meta, err = c.client.LedgerInformation(c.ctx)
		if err != nil {
			c.logger.Errorf("get meta: %s", err.Error())
			return
		}

		version, err := strconv.ParseUint(meta.LedgerVersion, 10, 64)
		if err != nil {
			c.logger.Errorf("parse ledger version: %s", err.Error())
			return
		}

		for v < version {
			v += 1

			txs, err := c.client.GetTransactions(c.ctx, v, 10, true)
			if err != nil {
				continue
			}

			for _, tx := range txs {
				version, err := strconv.ParseUint(tx.Version, 10, 64)
				if err != nil {
					c.logger.Errorf("parse ledger version: %s", err.Error())
					return
				}
				if version > v {
					v = version
				}

				if tx.Type != txTypeUser {
					continue
				}

				c.parseTransaction(&tx)
			}
		}
	}
}
