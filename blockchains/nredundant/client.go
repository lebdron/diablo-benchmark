package nredundant

import (
	"diablo-benchmark/core"
	"sync"

	"golang.org/x/sync/errgroup"
)

type BlockchainClient struct {
	clients []core.BlockchainClient
}

func (bc *BlockchainClient) DecodePayload(encoded []byte) (interface{}, error) {
	return bc.clients[0].DecodePayload(encoded)
}

type interaction struct {
	inner     core.Interaction
	threshold int

	lock                          sync.Mutex
	submitted, committed, aborted bool
	timesCommitted, timesAborted  int
}

func (i *interaction) Payload() interface{} {
	return i.inner.Payload()
}

func (i *interaction) ReportSubmit() {
	i.lock.Lock()
	if i.submitted {
		i.lock.Unlock()
		return
	}
	i.submitted = true
	i.lock.Unlock()
	i.inner.ReportSubmit()
}

func (i *interaction) ReportCommit() {
	i.lock.Lock()
	if i.committed {
		i.lock.Unlock()
		return
	}
	i.timesCommitted += 1
	if i.timesCommitted == i.threshold {
		i.committed = true
	}
	committed := i.committed
	i.lock.Unlock()
	if committed {
		i.inner.ReportCommit()
	}
}

func (i *interaction) ReportAbort() {
	i.lock.Lock()
	if i.aborted {
		i.lock.Unlock()
		return
	}
	i.timesAborted += 1
	if i.timesAborted == i.threshold {
		i.aborted = true
	}
	aborted := i.aborted
	i.lock.Unlock()
	if aborted {
		i.inner.ReportAbort()
	}
}

func (bc *BlockchainClient) TriggerInteraction(iact core.Interaction) error {
	iact = &interaction{inner: iact, threshold: len(bc.clients)}
	g := new(errgroup.Group)
	for _, client := range bc.clients {
		client := client
		g.Go(func() error {
			return client.TriggerInteraction(iact)
		})
	}
	return g.Wait()
}
