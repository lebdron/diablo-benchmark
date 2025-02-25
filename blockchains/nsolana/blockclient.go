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

func (bc *blockClient) getBlock(number uint64) error {
	var block *rpc.GetBlockResult
	var err error

	bc.logger.Tracef("poll new block (number = %d)", number)

	includeRewards := false
	for {
		bc.logger.Tracef("calling GetBlockWithOpts %v", number)
		block, err = bc.client.GetBlockWithOpts(
			bc.ctx,
			number,
			&rpc.GetBlockOpts{
				TransactionDetails: rpc.TransactionDetailsSignatures,
				Rewards:            &includeRewards,
				// should be safe as we request blocks for rooted slots
				Commitment: rpc.CommitmentConfirmed,
			})
		bc.logger.Tracef("returned from GetBlockWithOpts %v", number)

		// rooted slot should be available
		var rpcerr *jsonrpc.RPCError
		if errors.As(err, &rpcerr) && rpcerr.Code == -32004 {
			bc.logger.Tracef("GetBlockWithOpts error %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		break
	}
	if err != nil {
		return fmt.Errorf("GetBlockWithOpts failed: %w", err)
	}

	bc.blocks <- blockResult{block: block}

	return nil
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
		err := bc.getBlock(slot)
		var rpcerr *jsonrpc.RPCError
		if errors.As(err, &rpcerr) && (rpcerr.Code == -32001 || rpcerr.Code == -32007) {
			if rpcerr.Code == -32001 {
				slot, err = strconv.ParseUint(rpcerr.Message[strings.LastIndex(rpcerr.Message, " ")+1:], 10, 64)
				if err != nil {
					bc.blocks <- blockResult{err: fmt.Errorf("failed to parse slot: %w", err)}
					return
				}
			} else if rpcerr.Code == -32007 {
				slot += 1
			}
			bc.logger.Tracef("skipping to slot %v", slot)
			continue
		} else if errors.Is(err, rpc.ErrNotConfirmed) {
			bc.logger.Tracef("processBlock %v failed with %v, skipping", slot, err)
		} else if err != nil {
			bc.blocks <- blockResult{err: fmt.Errorf("processBlock failed: %w", err)}
			return
		}
		bc.logger.Tracef("processed slot %v", slot)
		slot++

		for subSlot < slot {
			event, err := bc.subscription.Recv()
			if err != nil {
				bc.blocks <- blockResult{err: fmt.Errorf("recv failed: %w", err)}
				return
			}
			if event == nil {
				bc.blocks <- blockResult{err: fmt.Errorf("received nil slot")}
				return
			}
			cand := uint64(*event)
			if cand > subSlot {
				subSlot = cand
			}
			bc.logger.Tracef("received root slot %v, new slot %v", cand, subSlot)
		}
	}
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
