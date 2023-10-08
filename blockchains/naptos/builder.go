package naptos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"diablo-benchmark/core"
	"fmt"
	"strconv"
	"time"

	aptosclient "github.com/portto/aptos-go-sdk/client"
	aptosmodels "github.com/portto/aptos-go-sdk/models"
)

type BlockchainBuilder struct {
	logger     core.Logger
	client     aptosclient.AptosClient
	mintSigner aptosmodels.SingleSigner
	// accountCreator  *accountCreator
	ctx             context.Context
	premadeAccounts []account
	usedAccounts    int
	builderAccount  *account
	compilers       []*moveCompiler
	applications    map[string]*application
	ownerAccounts   map[string]int
}

type account struct {
	signer   aptosmodels.SingleSigner
	sequence uint64
}

type contract struct {
	app  *application
	addr *account
}

// type accountCreator struct {
// 	client                             diemclient.Client
// 	mintKeys                           *diemkeys.Keys
// 	createChan, transferChan           chan diemkeys.AuthKey
// 	createWg, transferWg               sync.WaitGroup
// 	tcAccountAddress                   diemtypes.AccountAddress
// 	tcSequenceNumber, ddSequenceNumber uint64
// 	err                                error
// 	lock                               sync.RWMutex
// }

// func (this *accountCreator) createTxnToSubmit(accountAddress diemtypes.AccountAddress, sequenceNum uint64, script diemtypes.Script) *diemtypes.SignedTransaction {
// 	return diemsigner.Sign(
// 		this.mintKeys,
// 		accountAddress,
// 		sequenceNum,
// 		script,
// 		1_000_000, 0, "XUS",
// 		uint64(time.Now().Add(100*time.Second).Unix()),
// 		4,
// 	)
// }

// func (this *accountCreator) submitAndWait(sequenceNumber *uint64, address diemtypes.AccountAddress, script diemtypes.Script) error {
// 	new := atomic.AddUint64(sequenceNumber, 1)
// 	txn := this.createTxnToSubmit(address, new-1, script)
// 	err := this.client.SubmitTransaction(txn)
// 	if err != nil {
// 		return err
// 	}
// 	_, err = this.client.WaitForTransaction2(txn, 60*time.Second)
// 	if err != nil {
// 		return err
// 	}
// 	return nil
// }

// func (this *accountCreator) processCreate() {
// 	for key := range this.createChan {
// 		createAccount := stdlib.EncodeCreateParentVaspAccountScript(
// 			testnet.XUS,
// 			0,
// 			key.AccountAddress(),
// 			key.Prefix(),
// 			[]byte("testnet"),
// 			false)
// 		err := this.submitAndWait(&this.tcSequenceNumber, this.tcAccountAddress, createAccount)
// 		this.createWg.Done()
// 		if err != nil {
// 			this.lock.Lock()
// 			this.err = err
// 			this.lock.Unlock()
// 			continue
// 		}
// 		this.transferWg.Add(1)
// 		this.transferChan <- key
// 	}
// }

// func (this *accountCreator) processTransfer() {
// 	for key := range this.transferChan {
// 		transferXus := stdlib.EncodePeerToPeerWithMetadataScript(
// 			testnet.XUS,
// 			key.AccountAddress(),
// 			1_000_000,
// 			[]byte{},
// 			[]byte{})
// 		err := this.submitAndWait(&this.ddSequenceNumber, testnet.DDAccountAddress, transferXus)
// 		this.transferWg.Done()
// 		if err != nil {
// 			this.lock.Lock()
// 			this.err = err
// 			this.lock.Unlock()
// 			continue
// 		}
// 	}
// }

// func (this *accountCreator) createAccount(key diemkeys.AuthKey) error {
// 	this.lock.RLock()
// 	err := this.err
// 	this.lock.RUnlock()
// 	if err != nil {
// 		return err
// 	}
// 	this.createWg.Add(1)
// 	this.createChan <- key
// 	return nil
// }

// func (this *accountCreator) wait() error {
// 	this.createWg.Wait()
// 	close(this.createChan)
// 	this.transferWg.Wait()
// 	close(this.transferChan)
// 	this.lock.RLock()
// 	err := this.err
// 	this.lock.RUnlock()
// 	if err != nil {
// 		return err
// 	}
// 	return nil
// }

// func newAccountCreator(client diemclient.Client, mintKeys *diemkeys.Keys) (*accountCreator, error) {
// 	this := &accountCreator{}

// 	this.client = client
// 	this.mintKeys = mintKeys
// 	this.createChan = make(chan diemkeys.AuthKey)
// 	this.transferChan = make(chan diemkeys.AuthKey)
// 	this.tcAccountAddress = diemtypes.MustMakeAccountAddress("0000000000000000000000000B1E55ED")
// 	tcAccount, err := client.GetAccount(this.tcAccountAddress)
// 	if tcAccount == nil {
// 		return nil, fmt.Errorf("TC account missing")
// 	}
// 	if err != nil {
// 		return nil, err
// 	}
// 	this.tcSequenceNumber = tcAccount.SequenceNumber
// 	ddAccount, err := client.GetAccount(testnet.DDAccountAddress)
// 	if ddAccount == nil {
// 		return nil, fmt.Errorf("DD account missing")
// 	}
// 	if err != nil {
// 		return nil, err
// 	}
// 	this.ddSequenceNumber = ddAccount.SequenceNumber

// 	poolSize := 100
// 	for i := 0; i < poolSize; i++ {
// 		go this.processCreate()
// 		go this.processTransfer()
// 	}

// 	return this, nil
// }

func newBuilder(logger core.Logger, client aptosclient.AptosClient, mintSigner aptosmodels.SingleSigner, ctx context.Context) *BlockchainBuilder {
	return &BlockchainBuilder{
		logger:     logger,
		client:     client,
		mintSigner: mintSigner,
		// accountCreator:  nil,
		ctx:             ctx,
		premadeAccounts: make([]account, 0),
		usedAccounts:    0,
		builderAccount:  nil,
		compilers:       make([]*moveCompiler, 0),
		applications:    make(map[string]*application),
		ownerAccounts:   make(map[string]int),
	}
}

func (b *BlockchainBuilder) addAccount(key ed25519.PrivateKey) {
	b.premadeAccounts = append(b.premadeAccounts, account{
		signer:   aptosmodels.NewSingleSigner(key),
		sequence: 0,
	})
}

func (b *BlockchainBuilder) addCompiler(path string, stdlibs []string) {
	compiler := newMoveCompiler(b.logger, path, stdlibs)

	b.compilers = append(b.compilers, compiler)
}

func (b *BlockchainBuilder) initAccount(account *account) error {
	// var acc *diemjsonrpctypes.Account
	// var err error

	// if this.accountCreator != nil {
	// 	err = this.accountCreator.wait()
	// 	this.accountCreator = nil
	// 	if err != nil {
	// 		return err
	// 	}
	// }

	acc, err := b.client.GetAccount(b.ctx, account.signer.ToHex())
	if err != nil {
		return err
	}

	if acc == nil {
		return fmt.Errorf("account does not exist")
	}

	sequence, err := strconv.ParseUint(acc.SequenceNumber, 10, 64)
	if err != nil {
		return err
	}
	account.sequence = uint64(sequence)

	return nil
}

func (b *BlockchainBuilder) getAccount(index *int) (*account, error) {
	var ret *account
	var err error

	if *index < len(b.premadeAccounts) {
		ret = &b.premadeAccounts[*index]
		*index += 1

		err = b.initAccount(ret)
		if err != nil {
			return nil, err
		}
		// } else if *index == len(this.premadeAccounts) {
		// 	this.logger.Debugf("creating account %d", *index)
		// 	pk, sk, err := ed25519.GenerateKey(nil)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// 	this.addAccount(sk)
		// 	ret = &this.premadeAccounts[*index]
		// 	key := diemkeys.NewAuthKey(diemkeys.NewEd25519PublicKey(pk))
		// 	*index += 1
		// 	if this.accountCreator == nil {
		// 		this.accountCreator, err = newAccountCreator(this.client, this.mintKeys)
		// 		if err != nil {
		// 			return nil, err
		// 		}
		// 	}
		// 	err = this.accountCreator.createAccount(key)
		// 	if err != nil {
		// 		return nil, err
		// 	}
	} else {
		return nil, fmt.Errorf("unexpected index %d", *index)
	}

	return ret, nil
}

func (b *BlockchainBuilder) getBuilderAccount() (*account, error) {
	var index int = 0
	return b.getAccount(&index)
}

func (b *BlockchainBuilder) getOwnerAccount(name string) (*account, error) {
	var ret *account
	var index int
	var err error
	var ok bool

	index, ok = b.ownerAccounts[name]
	if !ok {
		index = 0
	}

	ret, err = b.getAccount(&index)
	b.ownerAccounts[name] = index

	return ret, err
}

func (b *BlockchainBuilder) CreateAccount(stake int) (interface{}, error) {
	return b.getAccount(&b.usedAccounts)
}

func (b *BlockchainBuilder) getApplication(name string) (*application, error) {
	var compiler *moveCompiler
	var appli *application
	var builder *account
	var err error
	var ok bool

	builder, err = b.getBuilderAccount()
	if err != nil {
		return nil, err
	}

	appli, ok = b.applications[name]
	if ok {
		return appli, nil
	}

	for _, compiler = range b.compilers {
		appli, err = compiler.compile(name, builder)

		if err == nil {
			break
		} else {
			b.logger.Debugf("failed to compile '%s': %s",
				name, err.Error())
		}
	}

	if appli == nil {
		return nil, fmt.Errorf("failed to compile contract '%s'", name)
	}

	b.applications[name] = appli

	return appli, nil
}

func (b *BlockchainBuilder) getDeployedApplication(name string) (*application, error) {
	var appli *application
	var err error

	appli, err = b.getApplication(name)
	if err != nil {
		return nil, err
	}

	if !appli.deployed {
		b.logger.Debugf("deploy new module '%s'", name)
		err = b.deployApplication(appli)
		if err != nil {
			return nil, err
		}

		appli.deployed = true
	}

	return appli, nil
}

func (b *BlockchainBuilder) deployApplication(appli *application) error {
	builder, err := b.getBuilderAccount()
	if err != nil {
		return err
	}

	tx := newDeployContractTransaction(builder.signer.PrivateKey, appli.moduleCode,
		builder.sequence)

	_, stx, err := tx.getSigned()
	if err != nil {
		return err
	}

	err = b.submitTransaction(stx)
	if err != nil {
		return err
	}

	return nil

}

func (b *BlockchainBuilder) CreateContract(name string) (interface{}, error) {
	appli, err := b.getDeployedApplication(name)
	if err != nil {
		return nil, err
	}

	owner, err := b.getOwnerAccount(name)
	if err != nil {
		return nil, err
	}

	tx := newInvokeTransaction(owner.signer.PrivateKey, appli.ctorCode,
		[]aptosmodels.TransactionArgument{}, owner.sequence)

	_, stx, err := tx.getSigned()
	if err != nil {
		return nil, err
	}

	b.logger.Tracef("construct new instance of '%s'", name)

	err = b.submitTransaction(stx)
	if err != nil {
		return nil, err
	}

	return &contract{
		app:  appli,
		addr: owner,
	}, nil
}

func (b *BlockchainBuilder) submitTransaction(stx *aptosmodels.UserTransaction) error {
	_, err := b.client.SubmitTransaction(b.ctx, *stx)
	if err != nil {
		return err
	}

	state, err := WaitForTransaction(b.client, b.ctx, stx, 30*time.Second)
	if err != nil {
		return err
	}

	if state.VmStatus != VmStatusExecuted {
		return fmt.Errorf("transaction failed to execute (%s)",
			state.VmStatus)
	}

	return nil
}

func (b *BlockchainBuilder) CreateResource(domain string) (core.SampleFactory, bool) {
	return nil, false
}

func (b *BlockchainBuilder) EncodeTransfer(amount int, from, to interface{}, info core.InteractionInfo) ([]byte, error) {
	// var tx *transferTransaction
	// var err error

	// if this.accountCreator != nil {
	// 	err = this.accountCreator.wait()
	// 	this.accountCreator = nil
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }

	tx := newTransferTransaction(from.(*account).signer.PrivateKey, to.(*account).signer.AccountAddress,
		uint64(amount), from.(*account).sequence)

	var buffer bytes.Buffer
	err := tx.encode(&buffer)
	if err != nil {
		return nil, err
	}

	from.(*account).sequence += 1

	return buffer.Bytes(), nil
}

func (b *BlockchainBuilder) EncodeInvoke(from interface{}, contr interface{}, function string, info core.InteractionInfo) ([]byte, error) {
	var args *applicationArguments
	var tx *invokeTransaction
	var buffer bytes.Buffer
	var cont *contract
	var err error

	// if this.accountCreator != nil {
	// 	err = this.accountCreator.wait()
	// 	this.accountCreator = nil
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// }

	cont = contr.(*contract)

	args, err = cont.app.arguments(function, cont.addr.signer.AccountAddress)
	if err != nil {
		return nil, err
	}

	tx = newInvokeTransaction(from.(*account).signer.PrivateKey, args.funccode,
		args.funcargs, from.(*account).sequence)

	err = tx.encode(&buffer)
	if err != nil {
		return nil, err
	}

	from.(*account).sequence += 1

	return buffer.Bytes(), nil
}

func (b *BlockchainBuilder) EncodeInteraction(itype string, expr core.BenchmarkExpression, info core.InteractionInfo) ([]byte, error) {
	return []byte{}, nil
}
