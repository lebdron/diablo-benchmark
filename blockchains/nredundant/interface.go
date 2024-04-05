package nredundant

import (
	"diablo-benchmark/core"
	"fmt"
	"maps"
	"strconv"
)

type BlockchainInterface struct {
	inner core.BlockchainInterface
}

func NewInterface(iface core.BlockchainInterface) core.BlockchainInterface {
	return &BlockchainInterface{iface}
}

func (bi *BlockchainInterface) Builder(params map[string]string, env []string, endpoints map[string][]string, logger core.Logger) (core.BlockchainBuilder, error) {
	return bi.inner.Builder(params, env, endpoints, logger)
}

func (bi *BlockchainInterface) Client(params map[string]string, env, view []string, logger core.Logger) (core.BlockchainClient, error) {
	var err error

	redundancy := uint64(1)
	if value, ok := params["redundancy"]; ok {
		params = maps.Clone(params)
		delete(params, "redundancy")
		redundancy, err = strconv.ParseUint(value, 10, 0)
		if err != nil {
			return nil, fmt.Errorf("invalid redundancy parameter: %v", value)
		}

		if redundancy == 0 {
			return nil, fmt.Errorf("invalid redundancy parameter: %v", redundancy)
		}
	}
	if len(view) < int(redundancy) {
		return nil, fmt.Errorf("less endpoints (%v) than redundancy (%v)", len(view), redundancy)
	}

	clients := make([]core.BlockchainClient, redundancy)
	for i, endpoint := range view[0:redundancy] {
		clients[i], err = bi.inner.Client(params, env, []string{endpoint}, logger)
		if err != nil {
			return nil, err
		}
	}

	return &BlockchainClient{clients}, nil
}
