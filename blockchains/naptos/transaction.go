package naptos

import (
	"crypto/ed25519"
	"diablo-benchmark/core"
	"diablo-benchmark/util"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"

	aptosmodels "github.com/portto/aptos-go-sdk/models"
	"github.com/the729/lcs"
	"golang.org/x/crypto/sha3"
)

func TransactionHash(t *aptosmodels.UserTransaction) (string, error) {
	var tx aptosmodels.TransactionEnum = *t
	bcsBytes, err := lcs.Marshal(&tx)
	if err != nil {
		return "", err
	}

	sha256 := sha3.New256()
	sha256.Write(aptosmodels.TransactionSalt[:])
	sha256.Write(bcsBytes)
	hash := sha256.Sum(nil)

	return "0x" + hex.EncodeToString(hash[:]), nil
}

const (
	transaction_type_transfer uint8 = 0
	transaction_type_invoke   uint8 = 1

	maximumGasAmount uint64 = 500_000
	gasUnitPrice     uint64 = 100

	expirationDelay = 86400 * time.Second
)

var (
	addr0x1, _ = aptosmodels.HexToAccountAddress("0x1")

	aptosCoinTypeTag = aptosmodels.TypeTagStruct{
		Address: addr0x1,
		Module:  "aptos_coin",
		Name:    "AptosCoin",
	}
)

type transaction interface {
	getSigned() (*aptosmodels.UserTransaction, error)
	getName() string
}

type lazyTransaction struct {
	getSignedOnce func() (*aptosmodels.UserTransaction, error)
	getNameOnce   func() string
}

func newLazyTransaction(inner transaction) *lazyTransaction {
	return &lazyTransaction{
		getSignedOnce: sync.OnceValues(func() (*aptosmodels.UserTransaction, error) {
			return inner.getSigned()
		}),
		getNameOnce: sync.OnceValue(func() string {
			return inner.getName()
		}),
	}
}

func (lt *lazyTransaction) getSigned() (*aptosmodels.UserTransaction, error) {
	return lt.getSignedOnce()
}

func (lt *lazyTransaction) getName() string {
	return lt.getNameOnce()
}

type outerTransaction struct {
	inner virtualTransaction
}

func (t *outerTransaction) getSigned() (*aptosmodels.UserTransaction, error) {
	inner, stx, err := t.inner.getSigned()

	t.inner = inner

	return stx, err
}

func (t *outerTransaction) getName() string {
	return t.inner.getName()
}

func decodeTransaction(src io.Reader) (transaction, error) {
	var inner virtualTransaction
	var txtype uint8
	var err error

	err = util.NewMonadInputReader(src).
		SetOrder(binary.LittleEndian).
		ReadUint8(&txtype).
		Error()
	if err != nil {
		return nil, err
	}

	switch txtype {
	case transaction_type_transfer:
		inner, err = decodeTransferTransaction(src)
	case transaction_type_invoke:
		inner, err = decodeInvokeTransaction(src)
	default:
		return nil, fmt.Errorf("unknown transaction type %d", txtype)
	}

	if err != nil {
		return nil, err
	}

	return newLazyTransaction(&outerTransaction{inner}), nil
}

type virtualTransaction interface {
	getSigned() (virtualTransaction, *aptosmodels.UserTransaction, error)
	getName() string
}

type signedTransaction struct {
	inner *aptosmodels.UserTransaction
	name  string
}

func newSignedTransaction(inner *aptosmodels.UserTransaction, name string) *signedTransaction {
	return &signedTransaction{
		inner: inner,
		name:  name,
	}
}

func (t *signedTransaction) getSigned() (virtualTransaction, *aptosmodels.UserTransaction, error) {
	return t, t.inner, nil
}

func (t *signedTransaction) getName() string {
	return t.name
}

type unsignedTransaction struct {
	from           aptosmodels.SingleSigner
	sequence       uint64
	payload        aptosmodels.TransactionPayload
	maxGasAmount   uint64
	gasUnitPrice   uint64
	expirationTime uint64
	chainId        byte
	name           string
}

func newUnsignedTransaction(from aptosmodels.SingleSigner, sequence uint64, payload aptosmodels.TransactionPayload, name string) *unsignedTransaction {
	return &unsignedTransaction{
		from:     from,
		sequence: sequence,
		payload:  payload,
		name:     name,
	}
}

func (t *unsignedTransaction) getSigned() (virtualTransaction, *aptosmodels.UserTransaction, error) {
	expiration := uint64(time.Now().Add(expirationDelay).Unix())

	tx := aptosmodels.Transaction{}
	err := tx.SetChainID(chainId).
		SetSender(t.from.ToHex()).
		SetPayload(t.payload).
		SetExpirationTimestampSecs(expiration).
		SetGasUnitPrice(gasUnitPrice).
		SetMaxGasAmount(maximumGasAmount).
		SetSequenceNumber(t.sequence).
		Error()
	if err != nil {
		core.Debugf("error at tx builder %v", err)
		return nil, nil, err
	}

	msgBytes, err := tx.GetSigningMessage()
	if err != nil {
		core.Debugf("error at GetSigningMessage %v", err)
		return nil, nil, err
	}

	signature := ed25519.Sign(t.from.PrivateKey, msgBytes)
	err = tx.SetAuthenticator(aptosmodels.TransactionAuthenticatorEd25519{
		PublicKey: t.from.PublicKey,
		Signature: signature,
	}).Error()
	if err != nil {
		core.Debugf("error at SetAuthenticator %v", err)
		return nil, nil, err
	}

	return newSignedTransaction(&tx.UserTransaction, t.name).getSigned()
}

func (t *unsignedTransaction) getName() string {
	return t.name
}

type transferTransaction struct {
	from     ed25519.PrivateKey
	to       aptosmodels.AccountAddress
	amount   uint64
	sequence uint64
}

func newTransferTransaction(from ed25519.PrivateKey, to aptosmodels.AccountAddress, amount, sequence uint64) *transferTransaction {
	return &transferTransaction{
		from:     from,
		to:       to,
		amount:   amount,
		sequence: sequence,
	}
}

func decodeTransferTransaction(src io.Reader) (*transferTransaction, error) {
	var amount, sequence uint64
	var fromSeed []byte
	var to aptosmodels.AccountAddress
	toAddr := to[:]

	err := util.NewMonadInputReader(src).
		SetOrder(binary.LittleEndian).
		ReadBytes(&fromSeed, ed25519.SeedSize).
		ReadBytes(&toAddr, len(to)).
		ReadUint64(&amount).
		ReadUint64(&sequence).
		Error()

	if err != nil {
		return nil, err
	}

	return newTransferTransaction(ed25519.NewKeyFromSeed(fromSeed), to,
		amount, sequence), nil
}

func (t *transferTransaction) encode(dest io.Writer) error {
	return util.NewMonadOutputWriter(dest).
		SetOrder(binary.LittleEndian).
		WriteUint8(transaction_type_transfer).
		WriteBytes(t.from.Seed()).
		WriteBytes(t.to[:]).
		WriteUint64(t.amount).
		WriteUint64(t.sequence).
		Error()
}

func (t *transferTransaction) getSigned() (virtualTransaction, *aptosmodels.UserTransaction, error) {
	payload := aptosmodels.EntryFunctionPayload{
		Module: aptosmodels.Module{
			Address: addr0x1,
			Name:    "coin",
		},
		Function:      "transfer",
		TypeArguments: []aptosmodels.TypeTag{aptosCoinTypeTag},
		Arguments:     []interface{}{t.to, t.amount},
	}

	return newUnsignedTransaction(aptosmodels.NewSingleSigner(t.from), t.sequence, payload, t.getName()).
		getSigned()
}

func (t *transferTransaction) getName() string {
	var seed []byte = t.from.Seed()

	return fmt.Sprintf("%02x%02x%02x%02x%02x%02x:%d", seed[0], seed[1],
		seed[2], seed[3], seed[4], seed[5], t.sequence)
}

type deployContractTransaction struct {
	from     ed25519.PrivateKey
	code     []byte
	sequence uint64
}

func newDeployContractTransaction(from ed25519.PrivateKey, code []byte, sequence uint64) *deployContractTransaction {
	return &deployContractTransaction{
		from:     from,
		code:     code,
		sequence: sequence,
	}
}

func (t *deployContractTransaction) getSigned() (virtualTransaction, *aptosmodels.UserTransaction, error) {
	payload := aptosmodels.ModuleBundlePayload{
		Modules: []struct{ Code []byte }{{Code: t.code}},
	}

	return newUnsignedTransaction(aptosmodels.NewSingleSigner(t.from), t.sequence, payload, t.getName()).
		getSigned()
}

func (t *deployContractTransaction) getName() string {
	var seed []byte = t.from.Seed()

	return fmt.Sprintf("%02x%02x%02x%02x%02x%02x:%d", seed[0], seed[1],
		seed[2], seed[3], seed[4], seed[5], t.sequence)
}

type invokeTransaction struct {
	from     ed25519.PrivateKey
	code     []byte
	args     []aptosmodels.TransactionArgument
	sequence uint64
}

func newInvokeTransaction(from ed25519.PrivateKey,
	code []byte,
	args []aptosmodels.TransactionArgument,
	sequence uint64) *invokeTransaction {
	return &invokeTransaction{
		from:     from,
		code:     code,
		args:     args,
		sequence: sequence,
	}
}

func decodeInvokeTransaction(src io.Reader) (*invokeTransaction, error) {
	var args []aptosmodels.TransactionArgument
	var input util.MonadInput
	var fromSeed, code []byte
	var sequence uint64
	var bargs [][]byte
	var err error
	var i, n int

	input = util.NewMonadInputReader(src).
		SetOrder(binary.LittleEndian).
		ReadBytes(&fromSeed, ed25519.SeedSize).
		ReadUint64(&sequence).
		ReadUint16(&n).
		ReadBytes(&code, n).
		ReadUint8(&n)

	bargs = make([][]byte, n)

	for i = range bargs {
		input.ReadUint16(&n).ReadBytes(&bargs[i], n)
	}

	err = input.Error()
	if err != nil {
		return nil, err
	}

	args = make([]aptosmodels.TransactionArgument, len(bargs))

	for i = range args {
		err := lcs.Unmarshal(bargs[i], &args[i])
		if err != nil {
			return nil, err
		}
	}

	return newInvokeTransaction(ed25519.NewKeyFromSeed(fromSeed), code,
		args, sequence), nil
}

func (t *invokeTransaction) encode(dest io.Writer) error {
	var err error

	if len(t.code) > 65535 {
		return fmt.Errorf("code too large (%d bytes)", len(t.code))
	}

	if len(t.args) > 255 {
		return fmt.Errorf("too many arguments (%d)", len(t.args))
	}

	bargs := make([][]byte, len(t.args))

	for i, arg := range t.args {
		bargs[i], err = lcs.Marshal(arg)
		if err != nil {
			return err
		} else if len(bargs[i]) > 65535 {
			return fmt.Errorf("invoke arguments %d is too long "+
				"(%d bytes)", i, len(bargs[i]))
		}
	}

	output := util.NewMonadOutputWriter(dest).
		SetOrder(binary.LittleEndian).
		WriteUint8(transaction_type_invoke).
		WriteBytes(t.from.Seed()).
		WriteUint64(t.sequence).
		WriteUint16(uint16(len(t.code))).
		WriteBytes(t.code).
		WriteUint8(uint8(len(bargs)))

	for i := range bargs {
		output.WriteUint16(uint16(len(bargs[i]))).WriteBytes(bargs[i])
	}

	return output.Error()
}

func (t *invokeTransaction) getSigned() (virtualTransaction, *aptosmodels.UserTransaction, error) {
	payload := aptosmodels.ScriptPayload{
		Code:          t.code,
		TypeArguments: []aptosmodels.TypeTag{},
		Arguments:     t.args,
	}

	return newUnsignedTransaction(aptosmodels.NewSingleSigner(t.from), t.sequence, payload, t.getName()).
		getSigned()
}

func (t *invokeTransaction) getName() string {
	seed := t.from.Seed()

	return fmt.Sprintf("%02x%02x%02x%02x%02x%02x:%d", seed[0], seed[1],
		seed[2], seed[3], seed[4], seed[5], t.sequence)
}
