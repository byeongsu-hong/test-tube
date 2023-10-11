package testenv

import (
	"encoding/json"
	"fmt"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"strings"
	"time"

	// helpers

	// tendermint
	"cosmossdk.io/errors"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
	tmtypes "github.com/tendermint/tendermint/proto/tendermint/types"
	dbm "github.com/tendermint/tm-db"

	// cosmos-sdk
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/simapp"
	sdk "github.com/cosmos/cosmos-sdk/types"
	slashingtypes "github.com/cosmos/cosmos-sdk/x/slashing/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"

	// wasmd
	"github.com/CosmWasm/wasmd/x/wasm"
	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"

	// ccv
	ccvconsumertypes "github.com/cosmos/interchain-security/x/ccv/consumer/types"

	// neutron
	"github.com/neutron-org/neutron/app"
	appparams "github.com/neutron-org/neutron/app/params"
	contractmanagermoduletypes "github.com/neutron-org/neutron/x/contractmanager/types"
	feeburnertypes "github.com/neutron-org/neutron/x/feeburner/types"
	feerefundertypes "github.com/neutron-org/neutron/x/feerefunder/types"
	interchainqueriesmoduletypes "github.com/neutron-org/neutron/x/interchainqueries/types"
	interchaintxstypes "github.com/neutron-org/neutron/x/interchaintxs/types"
)

type TestEnv struct {
	App                *app.App
	Ctx                sdk.Context
	ParamTypesRegistry ParamTypeRegistry
	ValPrivs           []*secp256k1.PrivKey
	NodeHome           string
}

// DebugAppOptions is a stub implementing AppOptions
type DebugAppOptions struct{}

// Get implements AppOptions
func (ao DebugAppOptions) Get(o string) interface{} {
	if o == server.FlagTrace {
		return true
	}
	return nil
}

func SetupNeutronApp(nodeHome string) *app.App {
	db := dbm.NewMemDB()

	/**
	encodingConfig appparams.EncodingConfig,
	enabledProposals []wasm.ProposalType,
	*/

	encCfg := appparams.MakeEncodingConfig()

	appInstance := app.New(
		log.NewNopLogger(),
		db,
		nil,
		true,
		map[int64]bool{},
		nodeHome,
		5,
		encCfg,
		[]wasm.ProposalType{},
		DebugAppOptions{},
		[]wasm.Option{},
	)

	genesisState := app.NewDefaultGenesisState(encCfg.Marshaler)

	// Set up Wasm genesis state
	wasmGen := wasm.GenesisState{
		Params: wasmtypes.Params{
			// Allow store code without gov
			CodeUploadAccess:             wasmtypes.AllowEverybody,
			InstantiateDefaultPermission: wasmtypes.AccessTypeEverybody,
		},
	}
	genesisState[wasm.ModuleName] = encCfg.Marshaler.MustMarshalJSON(&wasmGen)

	// Set up staking genesis state
	stakingParams := stakingtypes.DefaultParams()
	stakingParams.UnbondingTime = time.Hour * 24 * 7 * 2 // 2 weeks
	stakingGen := stakingtypes.GenesisState{
		Params: stakingParams,
	}
	genesisState[stakingtypes.ModuleName] = encCfg.Marshaler.MustMarshalJSON(&stakingGen)

	stateBytes, err := json.MarshalIndent(genesisState, "", " ")

	requireNoErr(err)

	concensusParams := simapp.DefaultConsensusParams
	concensusParams.Block = &abci.BlockParams{
		MaxBytes: 22020096,
		MaxGas:   -1,
	}

	// replace sdk.DefaultDenom with "uosmo", a bit of a hack, needs improvement
	stateBytes = []byte(strings.Replace(string(stateBytes), "\"stake\"", "\"uosmo\"", -1))

	appInstance.InitChain(
		abci.RequestInitChain{
			Validators:      []abci.ValidatorUpdate{},
			ConsensusParams: concensusParams,
			AppStateBytes:   stateBytes,
		},
	)

	return appInstance
}

func (env *TestEnv) BeginNewBlock(executeNextEpoch bool, timeIncreaseSeconds uint64) {
	var valAddr []byte

	validators := env.App.ConsumerKeeper.GetAllCCValidator(env.Ctx)
	if len(validators) >= 1 {
		valAddr = validators[0].GetAddress()
	} else {
		valPriv, valAddrFancy := env.setupValidator(stakingtypes.Bonded)
		validator, _ := env.App.ConsumerKeeper.GetCCValidator(env.Ctx, valAddrFancy)
		valAddr = validator.GetAddress()

		env.ValPrivs = append(env.ValPrivs, valPriv)
		err := simapp.FundAccount(
			env.App.BankKeeper, env.Ctx, valAddrFancy.Bytes(),
			sdk.NewCoins(sdk.NewInt64Coin("untrn", 9223372036854775807)),
		)
		if err != nil {
			panic(errors.Wrapf(err, "Failed to fund account"))
		}
	}

	env.beginNewBlockWithProposer(executeNextEpoch, valAddr, timeIncreaseSeconds)
}

func (env *TestEnv) GetValidatorAddresses() []string {
	validators := env.App.ConsumerKeeper.GetAllCCValidator(env.Ctx)
	var addresses []string
	for _, validator := range validators {
		addresses = append(addresses, sdk.ValAddress(validator.GetAddress()).String())
	}

	return addresses
}

// beginNewBlockWithProposer begins a new block with a proposer.
func (env *TestEnv) beginNewBlockWithProposer(
	executeNextEpoch bool, proposer sdk.ValAddress, timeIncreaseSeconds uint64,
) {
	validator, found := env.App.ConsumerKeeper.GetCCValidator(env.Ctx, proposer)

	if !found {
		panic("validator not found")
	}

	valAddr := validator.GetAddress()

	newBlockTime := env.Ctx.BlockTime().Add(time.Duration(timeIncreaseSeconds) * time.Second)
	header := tmtypes.Header{ChainID: "neutron-1", Height: env.Ctx.BlockHeight() + 1, Time: newBlockTime}
	newCtx := env.Ctx.WithBlockTime(newBlockTime).WithBlockHeight(env.Ctx.BlockHeight() + 1)
	env.Ctx = newCtx
	lastCommitInfo := abci.LastCommitInfo{
		Votes: []abci.VoteInfo{
			{
				Validator:       abci.Validator{Address: valAddr, Power: 1000},
				SignedLastBlock: true,
			},
		},
	}
	reqBeginBlock := abci.RequestBeginBlock{Header: header, LastCommitInfo: lastCommitInfo}

	env.App.BeginBlock(reqBeginBlock)
	env.Ctx = env.App.NewContext(false, reqBeginBlock.Header)
}

func (env *TestEnv) setupValidator(bondStatus stakingtypes.BondStatus) (*secp256k1.PrivKey, sdk.ValAddress) {
	valPriv := secp256k1.GenPrivKey()
	valPub := valPriv.PubKey()
	valAddr := sdk.ValAddress(valPub.Address())

	env.App.ConsumerKeeper.SetCCValidator(
		env.Ctx, ccvconsumertypes.CrossChainValidator{
			Address: valAddr.Bytes(),
			Power:   0,
			Pubkey: &codectypes.Any{
				TypeUrl: valPub.Type(),
				Value:   valPub.Bytes(),
			},
		},
	)

	signingInfo := slashingtypes.NewValidatorSigningInfo(
		sdk.GetConsAddress(valPub),
		env.Ctx.BlockHeight(),
		0,
		time.Unix(0, 0),
		false,
		0,
	)
	env.App.SlashingKeeper.SetValidatorSigningInfo(env.Ctx, sdk.GetConsAddress(valPub), signingInfo)

	return valPriv, valAddr
}

func (env *TestEnv) SetupParamTypes() {
	pReg := env.ParamTypesRegistry

	pReg.RegisterParamSet(&contractmanagermoduletypes.Params{})
	pReg.RegisterParamSet(&feeburnertypes.Params{})
	pReg.RegisterParamSet(&feerefundertypes.Params{})
	pReg.RegisterParamSet(&interchainqueriesmoduletypes.Params{})
	pReg.RegisterParamSet(&interchaintxstypes.Params{})
}

func requireNoErr(err error) {
	if err != nil {
		panic(err)
	}
}

func requireNoNil(name string, nilable any) {
	if nilable == nil {
		panic(fmt.Sprintf("%s must not be nil", name))
	}
}

func requierTrue(name string, b bool) {
	if !b {
		panic(fmt.Sprintf("%s must be true", name))
	}
}
