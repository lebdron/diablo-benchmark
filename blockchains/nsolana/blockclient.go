package nsolana

import (
	"context"
	"diablo-benchmark/core"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

const (
	RPCErrBlockNotAvailable = -32004
	RPCErrSlotSkipped       = -32007
	RPCErrBlockCleanedUp    = -32001
)

type blockResult struct {
	block *rpc.GetBlockResult
	err   error
}

type blockClient struct {
	logger       core.Logger
	client       *rpc.Client
	ctx          context.Context
	blocks       chan blockResult
	consumers    []chan<- blockResult
	subscription *ws.RootSubscription
}

func newBlockClient(logger core.Logger, client *rpc.Client, wsClient *ws.Client, ctx context.Context, consumers []chan<- blockResult) (*blockClient, error) {
	subscription, err := wsClient.RootSubscribe()
	if err != nil {
		return nil, fmt.Errorf("RootSubscribe failed: %w", err)
	}

	bc := &blockClient{
		logger:       logger,
		client:       client,
		ctx:          ctx,
		blocks:       make(chan blockResult, 500),
		consumers:    consumers,
		subscription: subscription,
	}

	go bc.broadcast()
	go bc.run()

	return bc, nil
}

func (bc *blockClient) getBlock(number uint64) (*rpc.GetBlockResult, error) {
	bc.logger.Tracef("poll new block (number = %d)", number)

	includeRewards := false
	opts := &rpc.GetBlockOpts{
		TransactionDetails: rpc.TransactionDetailsSignatures,
		Rewards:            &includeRewards,
		// should be safe as we request blocks for rooted slots
		Commitment: rpc.CommitmentConfirmed,
	}

	for {
		block, err := bc.client.GetBlockWithOpts(bc.ctx, number, opts)

		// rooted slot should be available
		var rpcerr *jsonrpc.RPCError
		if errors.As(err, &rpcerr) && rpcerr.Code == RPCErrBlockNotAvailable {
			bc.logger.Tracef("GetBlockWithOpts error %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("GetBlockWithOpts failed: %w", err)
		}
		return block, nil
	}
}

func (bc *blockClient) run() {
	defer bc.subscription.Unsubscribe()

	// slot, err := c.client.GetFirstAvailableBlock(c.ctx)
	// if err != nil {
	// 	return fmt.Errorf("GetFirstAvailableBlock failed: %w", err)
	// }
	slot := uint64(0)
	subSlot := slot

	for {
		block, err := bc.getBlock(slot)

		var rpcerr *jsonrpc.RPCError
		if errors.As(err, &rpcerr) {
			switch rpcerr.Code {
			case RPCErrBlockCleanedUp:
				numStr := rpcerr.Message[strings.LastIndex(rpcerr.Message, " ")+1:]
				slot, err = strconv.ParseUint(numStr, 10, 64)
				if err != nil {
					bc.blocks <- blockResult{err: fmt.Errorf("failed to parse slot: %w", err)}
					return
				}
				bc.logger.Tracef("skipping to slot %v", slot)
				continue
			case RPCErrSlotSkipped:
				slot += 1
				bc.logger.Tracef("skipping to slot %v", slot)
				continue
			}
		} else if errors.Is(err, rpc.ErrNotConfirmed) {
			bc.logger.Tracef("getBlock %v failed with %v, skipping", slot, err)
		} else if err != nil {
			bc.blocks <- blockResult{err: fmt.Errorf("getBlock failed: %w", err)}
			return
		} else {
			bc.logger.Tracef("received block %v", block.Blockhash)
			bc.blocks <- blockResult{block: block}
		}

		bc.logger.Tracef("processed slot %v", slot)
		slot++

		// Process subscription events to update subSlot
		if err := bc.updateSubSlot(&subSlot, slot); err != nil {
			bc.blocks <- blockResult{err: err}
			return
		}
	}
}

func (bc *blockClient) updateSubSlot(subSlot *uint64, slot uint64) error {
	for *subSlot < slot {
		event, err := bc.subscription.Recv()
		if err != nil {
			return fmt.Errorf("recv failed: %w", err)
		}
		if event == nil {
			return fmt.Errorf("received nil slot")
		}

		cand := uint64(*event)
		if cand > *subSlot {
			*subSlot = cand
		}
		bc.logger.Tracef("received root slot %v, new slot %v", cand, *subSlot)
	}
	return nil
}

func (bc *blockClient) broadcast() {
	for block := range bc.blocks {
		for _, consumer := range bc.consumers {
			select {
			case consumer <- block:
			default:
			}

		}

		if block.err != nil {
			for _, consumer := range bc.consumers {
				close(consumer)
			}
			bc.consumers = nil
			return
		}
	}
}
