package naptos

import (
	"context"
	"crypto/ed25519"
	"diablo-benchmark/core"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	aptosclient "github.com/portto/aptos-go-sdk/client"
	aptosmodels "github.com/portto/aptos-go-sdk/models"
	"gopkg.in/yaml.v3"
)

const chainId = 4

type BlockchainInterface struct {
}

func (i *BlockchainInterface) Builder(params map[string]string, env []string, endpoints map[string][]string, logger core.Logger) (core.BlockchainBuilder, error) {
	var key, value, endpoint string
	var envmap map[string][]string
	var builder *BlockchainBuilder
	var values []string
	var mintkey string
	var err error
	var ok bool

	logger.Debugf("new builder")

	envmap, err = parseEnvmap(env)
	if err != nil {
		return nil, err
	}

	for key = range endpoints {
		endpoint = key
		break
	}

	logger.Debugf("use endpoint '%s'", endpoint)
	client := aptosclient.NewAptosClient("http://" + endpoint)

	mintkey, ok = params["mintkey"]
	if !ok {
		return nil, fmt.Errorf("mintkey path missing")
	}
	delete(params, "mintkey")
	mintKeyBytes, err := os.ReadFile(mintkey)
	if err != nil {
		return nil, err
	}
	mintKeyBytes = mintKeyBytes[2:] // trim 0x
	n, err := hex.Decode(mintKeyBytes, mintKeyBytes)
	if err != nil {
		return nil, err
	}
	mintKeyBytes = mintKeyBytes[:n]
	mintKey := ed25519.NewKeyFromSeed(mintKeyBytes)
	mintSigner := aptosmodels.NewSingleSigner(mintKey)

	builder = newBuilder(logger, client, mintSigner, context.Background())

	for key, values = range envmap {
		if key == "accounts" {
			for _, value = range values {
				logger.Debugf("with accounts from '%s'", value)

				err = addPremadeAccounts(builder, value)
				if err != nil {
					return nil, err
				}
			}

			continue
		}

		if key == "contracts" {
			for _, value = range values {
				logger.Debugf("with contracts from '%s'", value)

				builder.addCompiler(value)
			}

			continue
		}

		return nil, fmt.Errorf("unknown environment key '%s'", key)
	}

	return builder, nil
}

func parseEnvmap(env []string) (map[string][]string, error) {
	var ret map[string][]string = make(map[string][]string)
	var element, key, value string
	var values []string
	var eqindex int
	var found bool

	for _, element = range env {
		eqindex = strings.Index(element, "=")
		if eqindex < 0 {
			return nil, fmt.Errorf("unexpected environment '%s'",
				element)
		}

		key = element[:eqindex]
		value = element[eqindex+1:]

		values, found = ret[key]
		if !found {
			values = make([]string, 0)
		}

		values = append(values, value)

		ret[key] = values
	}

	return ret, nil
}

func addPremadeAccounts(builder *BlockchainBuilder, path string) error {
	var decoder *yaml.Decoder
	var file *os.File
	var keys []string
	var seed []byte
	var key string
	var err error

	file, err = os.Open(path)
	if err != nil {
		return fmt.Errorf("addPremadeAccounts: failed to open file: %v", err)
	}

	decoder = yaml.NewDecoder(file)
	err = decoder.Decode(&keys)

	file.Close()

	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("addPremadeAccounts: failed to decode file: %v", err)
	}

	for _, key = range keys {
		seed, err = hex.DecodeString(key[2:])
		if err != nil {
			return fmt.Errorf("addPremadeAccounts: failed to decode hex key: %v", err)
		}

		builder.addAccount(ed25519.NewKeyFromSeed(seed))
	}

	return nil
}

func (i *BlockchainInterface) Client(params map[string]string, env, view []string, logger core.Logger) (core.BlockchainClient, error) {
	var confirmer transactionConfirmer
	var preparer transactionPreparer
	var key, value string
	var err error

	logger.Tracef("new client")

	logger.Tracef("use endpoint '%s'", view[0])
	client := aptosclient.NewAptosClient("http://" + view[0])

	for key, value = range params {
		if key == "confirm" {
			logger.Tracef("use confirm method '%s'", value)
			confirmer, err = parseConfirm(value, logger, client)
			if err != nil {
				return nil, err
			}
			continue
		}

		if key == "prepare" {
			logger.Tracef("use prepare method '%s'", value)
			preparer, err = parsePrepare(value, logger, client)
			if err != nil {
				return nil, err
			}
			continue
		}

		return nil, fmt.Errorf("unknown parameter '%s'", key)
	}

	if confirmer == nil {
		logger.Tracef("use default confirm method 'polltx'")
		confirmer = newPolltxTransactionConfirmer(logger, client)
	}

	if preparer == nil {
		logger.Tracef("use default prepare method 'signature'")
		preparer = newSignatureTransactionPreparer(logger)
	}

	return newClient(logger, client, preparer, confirmer), nil
}

func parseConfirm(value string, logger core.Logger, client aptosclient.AptosClient) (transactionConfirmer, error) {
	if value == "polltx" {
		return newPolltxTransactionConfirmer(logger, client), nil
	}

	if value == "pollblk" {
		return newPollblkTransactionConfirmer(logger, client), nil
	}

	return nil, fmt.Errorf("unknown confirm method '%s'", value)
}

func parsePrepare(value string, logger core.Logger, client aptosclient.AptosClient) (transactionPreparer, error) {
	var preparer transactionPreparer

	if value == "nothing" {
		preparer = newNothingTransactionPreparer()
		return preparer, nil
	}

	if value == "signature" {
		preparer = newSignatureTransactionPreparer(logger)
		return preparer, nil
	}

	return nil, fmt.Errorf("unknown prepare method '%s'", value)
}
