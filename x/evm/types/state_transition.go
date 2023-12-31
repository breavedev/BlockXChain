package types

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"

	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

// StateTransition defines data to transitionDB in evm
type StateTransition struct {
	// TxData fields
	AccountNonce uint64
	Price        *big.Int
	GasLimit     uint64
	Recipient    *common.Address
	Amount       *big.Int
	Payload      []byte

	ChainID  *big.Int
	Csdb     *CommitStateDB // state
	TxHash   *common.Hash
	Sender   common.Address
	Simulate bool // i.e CheckTx execution
}

// GasInfo returns the gas limit, gas consumed and gas refunded from the EVM transition
// execution
type GasInfo struct {
	GasLimit    uint64
	GasConsumed uint64
	GasRefunded uint64
}

// ExecutionResult represents what's returned from a transition
type ExecutionResult struct {
	Logs    []*ethtypes.Log
	Bloom   *big.Int
	Result  *sdk.Result
	GasInfo GasInfo
}

// GetHashFn implements vm.GetHashFunc for BlockX. It handles 3 cases:
//  1. The requested height matches the current height (and thus same epoch number)
//  2. The requested height is from an previous height from the same chain epoch
//  3. The requested height is from a height greater than the latest one
func GetHashFn(ctx sdk.Context, csdb *CommitStateDB) vm.GetHashFunc {
	return func(height uint64) common.Hash {
		switch {
		case ctx.BlockHeight() == int64(height):
			// Case 1: The requested height matches the one from the CommitStateDB so we can retrieve the block
			// hash directly from the CommitStateDB.
			return csdb.bhash

		case ctx.BlockHeight() > int64(height):
			// Case 2: if the chain is not the current height we need to retrieve the hash from the store for the
			// current chain epoch. This only applies if the current height is greater than the requested height.
			return csdb.WithContext(ctx).GetHeightHash(height)

		default:
			// Case 3: heights greater than the current one returns an empty hash.
			return common.Hash{}
		}
	}
}

func (st StateTransition) newEVM(
	ctx sdk.Context,
	csdb *CommitStateDB,
	gasLimit uint64,
	gasPrice *big.Int,
	config ChainConfig,
	extraEIPs []int64,
) *vm.EVM {
	// Create contexts for evm

	blockCtx := vm.BlockContext{
		CanTransfer: core.CanTransfer,
		Transfer:    core.Transfer,
		GetHash:     GetHashFn(ctx, csdb),
		Coinbase:    common.Address{}, // there's no beneficiary since we're not mining
		BlockNumber: big.NewInt(ctx.BlockHeight()),
		Time:        big.NewInt(ctx.BlockHeader().Time.Unix()),
		Difficulty:  big.NewInt(0), // unused. Only required in PoW context
		GasLimit:    gasLimit,
	}

	txCtx := vm.TxContext{
		Origin:   st.Sender,
		GasPrice: gasPrice,
	}

	eips := make([]int, len(extraEIPs))
	for i, eip := range extraEIPs {
		eips[i] = int(eip)
	}

	vmConfig := vm.Config{
		ExtraEips: eips,
	}

	return vm.NewEVM(blockCtx, txCtx, csdb, config.EthereumConfig(st.ChainID), vmConfig)
}

func (st StateTransition) refundGas(gasInfo GasInfo) {
	// Apply refund counter, capped to half of the used gas.
	refund := gasInfo.GasConsumed / 2
	if refund > gasInfo.GasRefunded {
		refund = gasInfo.GasRefunded
	}

	// Return ETH for remaining gas, exchanged at the original rate.
	gasRefund := new(big.Int).Mul(new(big.Int).SetUint64(refund), st.Price)

	// add refund back to counter.
	// add gas information into state db
	st.Csdb.AddRefund(refund)
	st.Csdb.AddBalance(st.Sender, gasRefund)

	// Also return remaining gas to the block gas counter so it is
	// available for the next transaction.
	// st.gp.AddGas(st.gas)
	// do we have a block gas counter?
	// will we need to track gas using gasmeter for the blocks?
}

// TransitionDb will transition the state by applying the current transaction and
// returning the evm execution result.
// NOTE: State transition checks are run during AnteHandler execution.
func (st StateTransition) TransitionDb(ctx sdk.Context, config ChainConfig) (*ExecutionResult, error) {
	contractCreation := st.Recipient == nil

	cost, err := core.IntrinsicGas(st.Payload, contractCreation, config.IsHomestead(), config.IsIstanbul())
	if err != nil {
		return nil, sdkerrors.Wrap(err, "invalid intrinsic gas for transaction")
	}

	// This gas limit the the transaction gas limit with intrinsic gas subtracted
	gasLimit := st.GasLimit - ctx.GasMeter().GasConsumed()

	csdb := st.Csdb.WithContext(ctx)
	if st.Simulate {
		// gasLimit is set here because stdTxs incur gaskv charges in the ante handler, but for eth_call
		// the cost needs to be the same as an Ethereum transaction sent through the web3 API
		consumedGas := ctx.GasMeter().GasConsumed()
		gasLimit = st.GasLimit - cost
		if consumedGas < cost {
			// If Cosmos standard tx ante handler cost is less than EVM intrinsic cost
			// gas must be consumed to match to accurately simulate an Ethereum transaction
			ctx.GasMeter().ConsumeGas(cost-consumedGas, "Intrinsic gas match")
		}

		csdb = st.Csdb.Copy()
	}

	// This gas meter is set up to consume gas from gaskv during evm execution and be ignored
	currentGasMeter := ctx.GasMeter()
	evmGasMeter := sdk.NewInfiniteGasMeter()
	csdb.WithContext(ctx.WithGasMeter(evmGasMeter))

	// Clear cache of accounts to handle changes outside of the EVM
	csdb.UpdateAccounts()

	params := csdb.GetParams()

	gasPrice := ctx.MinGasPrices().AmountOf(params.EvmDenom)
	if gasPrice.IsNil() {
		return nil, errors.New("gas price cannot be nil")
	}

	evm := st.newEVM(ctx, csdb, gasLimit, gasPrice.Int, config, params.ExtraEIPs)

	var (
		ret             []byte
		leftOverGas     uint64
		contractAddress common.Address
		recipientLog    string
		senderRef       = vm.AccountRef(st.Sender)
	)

	// Get nonce of account outside of the EVM
	currentNonce := csdb.GetNonce(st.Sender)
	// Set nonce of sender account before evm state transition for usage in generating Create address
	csdb.SetNonce(st.Sender, st.AccountNonce)

	// create contract or execute call
	switch contractCreation {
	case true:
		if !params.EnableCreate {
			return nil, ErrCreateDisabled
		}

		ret, contractAddress, leftOverGas, err = evm.Create(senderRef, st.Payload, gasLimit, st.Amount)
		recipientLog = fmt.Sprintf("contract address %s", contractAddress.String())
	default:
		if !params.EnableCall {
			return nil, ErrCallDisabled
		}

		// Increment the nonce for the next transaction	(just for evm state transition)
		csdb.SetNonce(st.Sender, csdb.GetNonce(st.Sender)+1)
		ret, leftOverGas, err = evm.Call(senderRef, *st.Recipient, st.Payload, gasLimit, st.Amount)
		recipientLog = fmt.Sprintf("recipient address %s", st.Recipient.String())
	}

	gasConsumed := gasLimit - leftOverGas

	if err != nil {
		// Consume gas before returning
		ctx.GasMeter().ConsumeGas(gasConsumed, "evm execution consumption")
		return nil, err
	}

	// Resets nonce to value pre state transition
	csdb.SetNonce(st.Sender, currentNonce)

	// Generate bloom filter to be saved in tx receipt data
	bloomInt := big.NewInt(0)

	var (
		bloomFilter ethtypes.Bloom
		logs        []*ethtypes.Log
	)

	if st.TxHash != nil && !st.Simulate {
		logs, err = csdb.GetLogs(*st.TxHash)
		if err != nil {
			return nil, err
		}

		bloomInt = big.NewInt(0).SetBytes(ethtypes.LogsBloom(logs))
		bloomFilter = ethtypes.BytesToBloom(bloomInt.Bytes())
	}

	if !st.Simulate {
		// Finalise state if not a simulated transaction
		// TODO: change to depend on config
		if err := csdb.Finalise(true); err != nil {
			return nil, err
		}
	}

	// Encode all necessary data into slice of bytes to return in sdk result
	resultData := ResultData{
		Bloom:  bloomFilter,
		Logs:   logs,
		Ret:    ret,
		TxHash: *st.TxHash,
	}

	if contractCreation {
		resultData.ContractAddress = contractAddress
	}

	resBz, err := EncodeResultData(resultData)
	if err != nil {
		return nil, err
	}

	resultLog := fmt.Sprintf(
		"executed EVM state transition; sender address %s; %s", st.Sender.String(), recipientLog,
	)

	// Consume gas from evm execution
	// Out of gas check does not need to be done here since it is done within the EVM execution
	ctx.WithGasMeter(currentGasMeter).GasMeter().ConsumeGas(gasConsumed, "EVM execution consumption")

	gasInfo := GasInfo{
		GasConsumed: gasConsumed,
		GasLimit:    gasLimit,
		GasRefunded: leftOverGas,
	}

	// refund gas to sender
	st.refundGas(gasInfo)

	executionResult := &ExecutionResult{
		Logs:  logs,
		Bloom: bloomInt,
		Result: &sdk.Result{
			Data: resBz,
			Log:  resultLog,
		},
		GasInfo: gasInfo,
	}

	return executionResult, nil
}
