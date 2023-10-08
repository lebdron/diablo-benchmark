module diablo-benchmark

go 1.14

require (
	github.com/algorand/go-algorand-sdk/v2 v2.2.0
	github.com/dfuse-io/logging v0.0.0-20210109005628-b97a57253f70 // indirect
	github.com/diem/client-sdk-go v1.2.1
	github.com/ethereum/go-ethereum v1.10.14
	github.com/gagliardetto/binary v0.7.7
	github.com/gagliardetto/solana-go v1.8.3
	github.com/hyperledger/fabric-sdk-go v1.0.0
	github.com/novifinancial/serde-reflection/serde-generate/runtime/golang v0.0.0-20201214184956-1fd02a932898
	github.com/portto/aptos-go-sdk v0.0.0-20230807103729-9a5201cad72f
	github.com/the729/lcs v0.1.5
	go.uber.org/zap v1.21.0
	golang.org/x/crypto v0.0.0-20220722155217-630584e8d5aa
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/portto/aptos-go-sdk => github.com/lebdron/aptos-go-sdk v0.0.0-20231007002036-aacfcea1bb02
