package nsolana

import (
	"diablo-benchmark/util"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gagliardetto/solana-go"
	bpfloader "github.com/gagliardetto/solana-go/programs/bpf-loader"
	"github.com/gagliardetto/solana-go/programs/system"
)

const (
	transaction_type_transfer uint8 = 0
	transaction_type_invoke   uint8 = 1

	default_ms_per_slot = 400
)

type parameters struct {
	blockhash solana.Hash
}

type transaction interface {
	getTx(parameters) (*solana.Transaction, error)
}

func decodeTransaction(src io.Reader) (transaction, error) {
	var txtype uint8
	err := util.NewMonadInputReader(src).
		SetOrder(binary.LittleEndian).
		ReadUint8(&txtype).
		Error()
	if err != nil {
		return nil, err
	}

	switch txtype {
	case transaction_type_transfer:
		return decodeTransferTransaction(src)
	case transaction_type_invoke:
		return decodeInvokeTransaction(src)
	default:
		return nil, fmt.Errorf("unknown transaction type %d", txtype)
	}
}

type baseTransaction struct {
	buildTx func(parameters) (*solana.Transaction, error)
	signers []solana.PrivateKey
}

func newBaseTransaction(
	buildTx func(parameters) (*solana.Transaction, error), signers []solana.PrivateKey) transaction {
	return &baseTransaction{buildTx, signers}
}

func (bt *baseTransaction) getTx(params parameters) (*solana.Transaction, error) {
	tx, err := bt.buildTx(params)
	if err != nil {
		return nil, err
	}

	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		for _, signer := range bt.signers {
			if signer.PublicKey() == key {
				return &signer
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return tx, nil
}

func newPlainTransaction(instructions []solana.Instruction, signers []solana.PrivateKey) transaction {
	buildTx := func(params parameters) (*solana.Transaction, error) {
		return solana.NewTransaction(instructions, params.blockhash)
	}
	return newBaseTransaction(buildTx, signers)
}

func newTransferTransaction(
	amount uint64, from solana.PrivateKey, to *solana.PublicKey) transaction {
	instruction := system.NewTransferInstruction(amount, from.PublicKey(), *to).Build()
	return newPlainTransaction([]solana.Instruction{instruction}, []solana.PrivateKey{from})
}

func decodeTransferTransaction(src io.Reader) (transaction, error) {
	var frombuf, tobuf []byte
	var amount uint64
	var lenkey int

	err := util.NewMonadInputReader(src).
		SetOrder(binary.LittleEndian).
		ReadUint8(&lenkey).
		ReadUint64(&amount).
		ReadBytes(&frombuf, lenkey).
		ReadBytes(&tobuf, solana.PublicKeyLength).
		Error()

	if err != nil {
		return nil, err
	}

	to := solana.PublicKeyFromBytes(tobuf)

	return newTransferTransaction(amount, frombuf, &to), nil
}

func encodeTransferTransaction(dest io.Writer, amount uint64, from solana.PrivateKey, to *solana.PublicKey) error {
	if len(from) > 255 {
		return fmt.Errorf("private key too long (%d bytes)", len(from))
	}

	return util.NewMonadOutputWriter(dest).
		SetOrder(binary.LittleEndian).
		WriteUint8(transaction_type_transfer).
		WriteUint8(uint8(len(from))).
		WriteUint64(amount).
		WriteBytes(from).
		WriteBytes(to.Bytes()).
		Error()
}

func newBuilderTransaction(builder *solana.TransactionBuilder, signers []solana.PrivateKey) transaction {
	buildTx := func(params parameters) (*solana.Transaction, error) {
		return builder.SetRecentBlockHash(params.blockhash).Build()
	}
	return newBaseTransaction(buildTx, signers)
}

func newDeployContractTransactionBatches(
	appli *application,
	from, program, storage *account,
	programLamports, storageLamports uint64) ([][]transaction, error) {
	// 1 - create program account
	// 2 - call loader writes
	// 3 - call loader finalize
	// 4 - create storage account and call contract constructor
	transactionBatches := make([][]transaction, 4)

	initialBuilder, writeBuilders, finalBuilder, _, err := bpfloader.Deploy(
		from.public, nil, appli.text, programLamports, solana.BPFLoaderProgramID, program.public, false)
	if err != nil {
		return nil, err
	}

	transactionBatches[0] = append(transactionBatches[0],
		newBuilderTransaction(initialBuilder, []solana.PrivateKey{from.private, program.private}))
	for _, builder := range writeBuilders {
		transactionBatches[1] = append(transactionBatches[1],
			newBuilderTransaction(builder, []solana.PrivateKey{from.private, program.private}))
	}
	transactionBatches[2] = append(transactionBatches[2],
		newBuilderTransaction(finalBuilder, []solana.PrivateKey{from.private, program.private}))

	// assuming that constructor does not have arguments
	{
		input, err := appli.abi.Constructor.Inputs.Pack()
		if err != nil {
			return nil, err
		}

		hash := crypto.Keccak256([]byte(appli.name))

		value := make([]byte, 8)
		binary.LittleEndian.PutUint64(value[0:], 0)

		data := []byte{}
		data = append(data, storage.public.Bytes()...)
		data = append(data, from.public.Bytes()...)
		data = append(data, value...)
		data = append(data, hash[:4]...)
		data = append(data, encodeSeeds()...)
		data = append(data, input...)

		builder := solana.NewTransactionBuilder().AddInstruction(
			system.NewCreateAccountInstruction(
				storageLamports,
				8192*8,
				program.public,
				from.public,
				storage.public).Build(),
		).AddInstruction(
			solana.NewInstruction(
				program.public,
				[]*solana.AccountMeta{
					solana.NewAccountMeta(
						storage.public,
						true,
						false),
				}, data),
		)
		transactionBatches[3] = append(transactionBatches[3],
			newBuilderTransaction(builder, []solana.PrivateKey{from.private, storage.private}))
	}

	return transactionBatches, nil
}

func newInvokeTransaction(
	amount uint64,
	from solana.PrivateKey,
	program, storage *solana.PublicKey,
	payload []byte) transaction {
	value := make([]byte, 8)
	binary.LittleEndian.PutUint64(value[0:], amount)

	data := []byte{}
	data = append(data, storage.Bytes()...)
	data = append(data, from.PublicKey().Bytes()...)
	data = append(data, value...)
	data = append(data, make([]byte, 4)...)
	data = append(data, encodeSeeds()...)
	data = append(data, payload...)

	instruction := solana.NewInstruction(
		*program,
		[]*solana.AccountMeta{
			solana.NewAccountMeta(from.PublicKey(), true, true),
			solana.NewAccountMeta(*storage, true, false),
			solana.NewAccountMeta(solana.SysVarClockPubkey, false, false),
			solana.NewAccountMeta(solana.PublicKey{}, false, false),
		}, data)

	return newPlainTransaction([]solana.Instruction{instruction}, []solana.PrivateKey{from})
}

func decodeInvokeTransaction(src io.Reader) (transaction, error) {
	var frombuf, programbuf, storagebuf, payload []byte
	var amount uint64
	var lenfrom, lenpayload int

	err := util.NewMonadInputReader(src).
		SetOrder(binary.LittleEndian).
		ReadUint8(&lenfrom).
		ReadUint16(&lenpayload).
		ReadUint64(&amount).
		ReadBytes(&frombuf, lenfrom).
		ReadBytes(&programbuf, solana.PublicKeyLength).
		ReadBytes(&storagebuf, solana.PublicKeyLength).
		ReadBytes(&payload, lenpayload).
		Error()

	if err != nil {
		return nil, err
	}

	program := solana.PublicKeyFromBytes(programbuf)
	storage := solana.PublicKeyFromBytes(storagebuf)

	return newInvokeTransaction(amount, frombuf, &program, &storage, payload), nil
}

func encodeInvokeTransaction(
	dest io.Writer, amount uint64, from solana.PrivateKey, program, storage *solana.PublicKey, payload []byte) error {
	if len(from) > 255 {
		return fmt.Errorf("private key too long (%d bytes)", len(from))
	}

	if len(payload) > 65535 {
		return fmt.Errorf("arguments too large (%d bytes)", len(payload))
	}

	return util.NewMonadOutputWriter(dest).
		SetOrder(binary.LittleEndian).
		WriteUint8(transaction_type_invoke).
		WriteUint8(uint8(len(from))).
		WriteUint16(uint16(len(payload))).
		WriteUint64(amount).
		WriteBytes(from).
		WriteBytes(program.Bytes()).
		WriteBytes(storage.Bytes()).
		WriteBytes(payload).
		Error()
}

type solangSeed struct {
	seed []byte
	// address solana.PublicKey
}

func encodeSeeds(seeds ...solangSeed) []byte {
	var length uint64 = 1
	for _, seed := range seeds {
		length += uint64(len(seed.seed)) + 1
	}
	seedEncoded := make([]byte, 0, length)

	seedEncoded = append(seedEncoded, uint8(len(seeds)))
	for _, seed := range seeds {
		seedEncoded = append(seedEncoded, uint8(len(seed.seed)))
		seedEncoded = append(seedEncoded, seed.seed...)
	}

	return seedEncoded
}
