package coverage

import (
	"math/big"
	"math/bits"

	"github.com/crytic/medusa/chain/types"
	"github.com/crytic/medusa/logging"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/holiman/uint256"
)

// coverageTracerResultsKey describes the key to use when storing tracer results in call message results, or when
// querying them.
const coverageTracerResultsKey = "CoverageTracerResults"

// GetCoverageTracerResults obtains CoverageMaps stored by a CoverageTracer from message results. This is nil if
// no CoverageMaps were recorded by a tracer (e.g. CoverageTracer was not attached during this message execution).
func GetCoverageTracerResults(messageResults *types.MessageResults) *CoverageMaps {
	// Try to obtain the results the tracer should've stored.
	if genericResult, ok := messageResults.AdditionalResults[coverageTracerResultsKey]; ok {
		if castedResult, ok := genericResult.(*CoverageMaps); ok {
			return castedResult
		}
	}

	// If we could not obtain them, return nil.
	return nil
}

// RemoveCoverageTracerResults removes CoverageMaps stored by a CoverageTracer from message results.
func RemoveCoverageTracerResults(messageResults *types.MessageResults) {
	delete(messageResults.AdditionalResults, coverageTracerResultsKey)
}

// CoverageTracer implements vm.EVMLogger to collect information such as coverage maps
// for fuzzing campaigns from EVM execution traces.
type CoverageTracer struct {
	// coverageMaps describes the execution coverage recorded. Call frames which errored are not recorded.
	coverageMaps *CoverageMaps

	// callFrameStates describes the state tracked by the tracer per call frame.
	callFrameStates []*coverageTracerCallFrameState

	// callDepth refers to the current EVM depth during tracing.
	callDepth uint64
}

// coverageTracerCallFrameState tracks state across call frames in the tracer.
type coverageTracerCallFrameState struct {
	// create indicates whether the current call frame is executing on init bytecode (deploying a contract).
	create bool

	// pendingCoverageMap describes the coverage maps recorded for this call frame.
	pendingCoverageMap *CoverageMaps

	// lookupHash describes the hash used to look up the ContractCoverageMap being updated in this frame.
	lookupHash *common.Hash
}

// NewCoverageTracer returns a new CoverageTracer.
func NewCoverageTracer() *CoverageTracer {
	tracer := &CoverageTracer{
		coverageMaps:    NewCoverageMaps(),
		callFrameStates: make([]*coverageTracerCallFrameState, 0),
	}
	return tracer
}

// CaptureTxStart is called upon the start of transaction execution, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureTxStart(gasLimit uint64) {
	// Reset our call frame states
	t.callDepth = 0
	t.coverageMaps = NewCoverageMaps()
	t.callFrameStates = make([]*coverageTracerCallFrameState, 0)
}

// CaptureTxEnd is called upon the end of transaction execution, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureTxEnd(restGas uint64) {
}

// CaptureStart initializes the tracing operation for the top of a call frame, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	// Create our state tracking struct for this frame.
	t.callFrameStates = append(t.callFrameStates, &coverageTracerCallFrameState{
		create:             create,
		pendingCoverageMap: NewCoverageMaps(),
	})
}

// CaptureEnd is called after a call to finalize tracing completes for the top of a call frame, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	// If we encountered an error in this call frame, mark all coverage as reverted.
	if err != nil {
		_, revertCoverageErr := t.callFrameStates[t.callDepth].pendingCoverageMap.RevertAll()
		if revertCoverageErr != nil {
			logging.GlobalLogger.Panic("Coverage tracer failed to update revert coverage map during capture end", revertCoverageErr)
		}
	}

	// Commit all our coverage maps up one call frame.
	_, _, coverageUpdateErr := t.coverageMaps.Update(t.callFrameStates[t.callDepth].pendingCoverageMap)
	if coverageUpdateErr != nil {
		logging.GlobalLogger.Panic("Coverage tracer failed to update coverage map during capture end", coverageUpdateErr)
	}

	// Pop the state tracking struct for this call frame off the stack.
	t.callFrameStates = t.callFrameStates[:t.callDepth]
}

// CaptureEnter is called upon entering of the call frame, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	// Increase our call depth now that we're entering a new call frame.
	t.callDepth++

	// Create our state tracking struct for this frame.
	t.callFrameStates = append(t.callFrameStates, &coverageTracerCallFrameState{
		create:             typ == vm.CREATE || typ == vm.CREATE2,
		pendingCoverageMap: NewCoverageMaps(),
	})
}

// CaptureExit is called upon exiting of the call frame, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	// If we encountered an error in this call frame, mark all coverage as reverted.
	if err != nil {
		_, revertCoverageErr := t.callFrameStates[t.callDepth].pendingCoverageMap.RevertAll()
		if revertCoverageErr != nil {
			logging.GlobalLogger.Panic("Coverage tracer failed to update revert coverage map during capture exit", revertCoverageErr)
		}
	}

	// Commit all our coverage maps up one call frame.
	_, _, coverageUpdateErr := t.callFrameStates[t.callDepth-1].pendingCoverageMap.Update(t.callFrameStates[t.callDepth].pendingCoverageMap)
	if coverageUpdateErr != nil {
		logging.GlobalLogger.Panic("Coverage tracer failed to update coverage map during capture exit", coverageUpdateErr)
	}

	// Pop the state tracking struct for this call frame off the stack.
	t.callFrameStates = t.callFrameStates[:t.callDepth]

	// Decrease our call depth now that we've exited a call frame.
	t.callDepth--
}

// CaptureState records data from an EVM state update, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, vmErr error) {
	// Obtain our call frame state tracking struct
	callFrameState := t.callFrameStates[t.callDepth]
	// If there is code we're executing, collect coverage.
	var pos *uint256.Int

	if op == vm.JUMPI {
		pos = scope.Stack.Back(0)
		cond := scope.Stack.Back(1)
		if !cond.IsZero() {
			pos = new(uint256.Int).Add(uint256.NewInt(pc), uint256.NewInt(1))
			// panic(pos)
		}
	} else if op == vm.JUMP {
		pos = scope.Stack.Back(0)
	} else {
		return
	}
	// Obtain our contract coverage map lookup hash.
	if callFrameState.lookupHash == nil {
		// TODO use scope.Contract.CodeHash??
		lookupHash := getContractCoverageMapHash(scope.Contract.Code, callFrameState.create)
		callFrameState.lookupHash = &lookupHash
	}
	// var uuid big.Int

	// uuid.Xor(scope.Contract.Address().Big(), callFrameState.lookupHash.Big())
	if pos != nil {

		marker := bits.RotateLeft64(pc, 32) ^ pos.Uint64()
		//logging.GlobalLogger.Info("tracer: pc ", pc, " pos ", pos, "op ", op, " marker", marker)

		// Record coverage for this location in our map.
		_, coverageUpdateErr := callFrameState.pendingCoverageMap.SetAt(scope.Contract.Address(), *callFrameState.lookupHash, len(scope.Contract.Code), marker)
		if coverageUpdateErr != nil {
			logging.GlobalLogger.Panic("Coverage tracer failed to update coverage map while tracing state", coverageUpdateErr)
		}
	}
}

// CaptureFault records an execution fault, as defined by vm.EVMLogger.
func (t *CoverageTracer) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {
}

// CaptureTxEndSetAdditionalResults can be used to set additional results captured from execution tracing. If this
// tracer is used during transaction execution (block creation), the results can later be queried from the block.
// This method will only be called on the added tracer if it implements the extended TestChainTracer interface.
func (t *CoverageTracer) CaptureTxEndSetAdditionalResults(results *types.MessageResults) {
	// Store our tracer results.
	results.AdditionalResults[coverageTracerResultsKey] = t.coverageMaps
}
