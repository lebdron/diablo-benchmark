package nsolana

import (
	"diablo-benchmark/core"
	"fmt"

	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

type blockResult struct {
	result *ws.BlockResult
	err    error
}

type blockSubscriber struct {
	logger       core.Logger
	subscription *ws.BlockSubscription
	blocks       chan blockResult
	consumers    []chan<- blockResult
}

func newBlockSubscriber(logger core.Logger, wsClient *ws.Client, consumers []chan<- blockResult) (*blockSubscriber, error) {
	includeRewards := false
	opts := &ws.BlockSubscribeOpts{
		Commitment:         rpc.CommitmentFinalized,
		TransactionDetails: rpc.TransactionDetailsSignatures,
		Rewards:            &includeRewards,
	}
	subscription, err := wsClient.BlockSubscribe(ws.NewBlockSubscribeFilterAll(), opts)
	if err != nil {
		return nil, fmt.Errorf("RootSubscribe failed: %w", err)
	}

	bs := &blockSubscriber{
		logger:       logger,
		subscription: subscription,
		blocks:       make(chan blockResult, 1000),
		consumers:    consumers,
	}

	go bs.broadcast()
	go bs.run()

	return bs, nil
}

func (bs *blockSubscriber) broadcast() {
	for update := range bs.blocks {
		for i, consumer := range bs.consumers {
			select {
			case consumer <- update:
				// Sent successfully
			default:
				// Non-blocking send failed
				bs.logger.Warnf("Consumer %d's buffer is full, skipping update", i)
			}
		}

		if update.err != nil {
			for _, consumer := range bs.consumers {
				close(consumer)
			}
			bs.consumers = nil
			return
		}
	}
}

// run processes incoming root slot notifications
func (bs *blockSubscriber) run() {
	defer bs.subscription.Unsubscribe()

	for {
		result, err := bs.subscription.Recv()
		if err != nil {
			bs.blocks <- blockResult{err: fmt.Errorf("block recv failed: %w", err)}
			return
		}
		if result == nil {
			bs.blocks <- blockResult{err: fmt.Errorf("received nil block")}
			return
		}

		bs.blocks <- blockResult{result: result}
	}
}
