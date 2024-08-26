package fuzzing

import (
	"fmt"
	"math/big"
	"math/rand"

	"github.com/crytic/medusa/chain"
	"github.com/crytic/medusa/fuzzing/calls"
	fuzzerTypes "github.com/crytic/medusa/fuzzing/contracts"
	"github.com/crytic/medusa/fuzzing/coverage"
	"github.com/crytic/medusa/fuzzing/valuegeneration"
	"github.com/crytic/medusa/utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"golang.org/x/exp/maps"
)

// FuzzerWorker describes a single thread worker utilizing its own go-ethereum test node to run property tests against
// Fuzzer-generated transaction sequences.
type FuzzerWorker struct {
	// workerIndex describes the index of the worker spun up by the fuzzer.
	workerIndex int
	// fuzzer describes the Fuzzer instance which this worker belongs to.
	fuzzer *Fuzzer

	// chain describes a test chain created by the FuzzerWorker to deploy contracts and run tests against.
	chain *chain.TestChain
	// coverageTracer describes the tracer used to collect coverage maps during fuzzing campaigns.
	coverageTracer *coverage.CoverageTracer

	// testingBaseBlockNumber refers to the block number at which all contracts for testing have been deployed, prior
	// to any fuzzing activity. This block number is reverted to after testing each call sequence to reset state.
	testingBaseBlockNumber uint64

	// deployedContracts describes a mapping of deployed contractDefinitions and the addresses they were deployed to.
	deployedContracts map[common.Address]*fuzzerTypes.Contract

	// stateChangingMethods is a list of contract functions which are suspected of changing contract state
	// (non-read-only). A sequence of calls is generated by the FuzzerWorker, targeting stateChangingMethods
	// before executing tests.
	stateChangingMethods []fuzzerTypes.DeployedContractMethod

	// pureMethods is a list of contract functions which are side-effect free with respect to the EVM (view and/or pure in terms of Solidity mutability).
	pureMethods []fuzzerTypes.DeployedContractMethod

	// randomProvider provides random data as inputs to decisions throughout the worker.
	randomProvider *rand.Rand
	// sequenceGenerator creates entirely new or mutated call sequences based on corpus call sequences, for use in
	// fuzzing campaigns.
	sequenceGenerator *CallSequenceGenerator

	// shrinkingValueMutator is a value mutator which is used to mutate existing call sequence values in an attempt to shrink
	// their values, in the call sequence shrinking process.
	shrinkingValueMutator valuegeneration.ValueMutator

	// valueSet defines a set derived from Fuzzer.BaseValueSet which is further populated with runtime values by the
	// FuzzerWorker. It is the value set shared with the underlying valueGenerator.
	valueSet *valuegeneration.ValueSet

	// Events describes the event system for the FuzzerWorker.
	Events FuzzerWorkerEvents
}

// newFuzzerWorker creates a new FuzzerWorker, assigning it the provided worker index/id and associating it to the
// Fuzzer instance supplied.
// Returns the new FuzzerWorker
func newFuzzerWorker(fuzzer *Fuzzer, workerIndex int, randomProvider *rand.Rand) (*FuzzerWorker, error) {
	// Clone the fuzzer's base value set, so we can build on it with runtime values.
	valueSet := fuzzer.baseValueSet.Clone()

	// Create a config for our call sequence generator for this new worker.
	callSequenceGenConfig, err := fuzzer.Hooks.NewCallSequenceGeneratorConfigFunc(fuzzer, valueSet, randomProvider)
	if err != nil {
		return nil, err
	}

	// Create a new shrinking value mutator for this new worker.
	shrinkingValueMutator, err := fuzzer.Hooks.NewShrinkingValueMutatorFunc(fuzzer, valueSet, randomProvider)
	if err != nil {
		return nil, err
	}

	// Create a new worker with the data provided.
	worker := &FuzzerWorker{
		workerIndex:          workerIndex,
		fuzzer:               fuzzer,
		deployedContracts:    make(map[common.Address]*fuzzerTypes.Contract),
		stateChangingMethods: make([]fuzzerTypes.DeployedContractMethod, 0),
		pureMethods:          make([]fuzzerTypes.DeployedContractMethod, 0),
		coverageTracer:       nil,
		randomProvider:       randomProvider,
		valueSet:             valueSet,
	}
	worker.sequenceGenerator = NewCallSequenceGenerator(worker, callSequenceGenConfig)
	worker.shrinkingValueMutator = shrinkingValueMutator

	return worker, nil
}

// WorkerIndex returns the index of this FuzzerWorker in relation to its parent Fuzzer.
func (fw *FuzzerWorker) WorkerIndex() int {
	return fw.workerIndex
}

// workerMetrics returns the fuzzerWorkerMetrics for this specific worker.
func (fw *FuzzerWorker) workerMetrics() *fuzzerWorkerMetrics {
	return &fw.fuzzer.metrics.workerMetrics[fw.workerIndex]
}

// Fuzzer returns the parent Fuzzer which spawned this FuzzerWorker.
func (fw *FuzzerWorker) Fuzzer() *Fuzzer {
	return fw.fuzzer
}

// Chain returns the Chain used by this worker as the backend for tests.
func (fw *FuzzerWorker) Chain() *chain.TestChain {
	return fw.chain
}

// DeployedContracts returns a mapping of deployed contract addresses to contract definitions, which are currently known
// to the fuzzer.
func (fw *FuzzerWorker) DeployedContracts() map[common.Address]*fuzzerTypes.Contract {
	// Return a clone of the map, as we don't want external usage of this to break it.
	return maps.Clone(fw.deployedContracts)
}

// DeployedContract obtains a contract deployed at the given address. If it does not exist, it returns nil.
func (fw *FuzzerWorker) DeployedContract(address common.Address) *fuzzerTypes.Contract {
	if contractDefinition, ok := fw.deployedContracts[address]; ok {
		return contractDefinition
	}
	return nil
}

// ValueSet obtains the value set used to power the value generator for this worker.
func (fw *FuzzerWorker) ValueSet() *valuegeneration.ValueSet {
	return fw.valueSet
}

// ValueGenerator obtains the value generator used by this worker.
func (fw *FuzzerWorker) ValueGenerator() valuegeneration.ValueGenerator {
	return fw.sequenceGenerator.config.ValueGenerator
}

// ValueMutator obtains the value mutator used by this worker.
func (fw *FuzzerWorker) ValueMutator() valuegeneration.ValueMutator {
	return fw.sequenceGenerator.config.ValueMutator
}

// getNewCorpusCallSequenceWeight returns a big integer representing the weight that a new corpus item being added now
// should have in the corpus' weighted random chooser.
func (fw *FuzzerWorker) getNewCorpusCallSequenceWeight() *big.Int {
	// Return our weight, ensuring it is non-zero.
	return new(big.Int).Add(fw.workerMetrics().sequencesTested, big.NewInt(1))
}

// onChainContractDeploymentAddedEvent is the event callback used when the chain detects a new contract deployment.
// It attempts bytecode matching and updates the list of deployed contracts the worker should use for fuzz testing.
func (fw *FuzzerWorker) onChainContractDeploymentAddedEvent(event chain.ContractDeploymentsAddedEvent) error {
	// Do not track the deployed contract if the contract deployment was a dynamic one and testAllContracts is false
	if !fw.fuzzer.config.Fuzzing.Testing.TestAllContracts && event.DynamicDeployment {
		return nil
	}

	// Add the contract address to our value set so our generator can use it in calls.
	fw.valueSet.AddAddress(event.Contract.Address)

	// Try to match it to a known contract definition
	matchedDefinition := fw.fuzzer.contractDefinitions.MatchBytecode(event.Contract.InitBytecode, event.Contract.RuntimeBytecode)
	// If we didn't match any deployment, report it.
	if matchedDefinition == nil {
		if fw.fuzzer.config.Fuzzing.Testing.StopOnFailedContractMatching {
			return fmt.Errorf("could not match bytecode of a deployed contract to any contract definition known to the fuzzer")
		} else {
			return nil
		}
	}

	// Set our deployed contract address in our deployed contract lookup, so we can reference it later.
	fw.deployedContracts[event.Contract.Address] = matchedDefinition

	// Update our methods
	fw.updateMethods()

	// Emit an event indicating the worker detected a new contract deployment on its chain.
	err := fw.Events.ContractAdded.Publish(FuzzerWorkerContractAddedEvent{
		Worker:             fw,
		ContractAddress:    event.Contract.Address,
		ContractDefinition: matchedDefinition,
	})
	if err != nil {
		return fmt.Errorf("error returned by an event handler when a worker emitted a deployed contract added event: %v", err)
	}
	return nil
}

// onChainContractDeploymentRemovedEvent is the event callback used when the chain detects removal of a previously
// deployed contract. It updates the list of deployed contracts the worker should use for fuzz testing.
func (fw *FuzzerWorker) onChainContractDeploymentRemovedEvent(event chain.ContractDeploymentsRemovedEvent) error {
	// Remove the contract address from our value set so our generator doesn't use it any longer
	fw.valueSet.RemoveAddress(event.Contract.Address)

	// Obtain our contract definition for this address. If we didn't record this contract deployment in the first place,
	// there is nothing to remove, so we exit early.
	contractDefinition, previouslyRegistered := fw.deployedContracts[event.Contract.Address]
	if !previouslyRegistered {
		return nil
	}

	// Remove the contract from our deployed contracts mapping the worker maintains.
	delete(fw.deployedContracts, event.Contract.Address)

	// Update our methods
	fw.updateMethods()

	// Emit an event indicating the worker detected the removal of a previously deployed contract on its chain.
	err := fw.Events.ContractDeleted.Publish(FuzzerWorkerContractDeletedEvent{
		Worker:             fw,
		ContractAddress:    event.Contract.Address,
		ContractDefinition: contractDefinition,
	})
	if err != nil {
		return fmt.Errorf("error returned by an event handler when a worker emitted a deployed contract deleted event: %v", err)
	}
	return nil
}

// updateMethods updates the list of methods used by the worker by re-evaluating them
// from the deployedContracts lookup.
func (fw *FuzzerWorker) updateMethods() {
	// Clear our list of methods
	fw.stateChangingMethods = make([]fuzzerTypes.DeployedContractMethod, 0)
	fw.pureMethods = make([]fuzzerTypes.DeployedContractMethod, 0)

	// Loop through each deployed contract
	for contractAddress, contractDefinition := range fw.deployedContracts {
		// If we deployed the contract, also enumerate property tests and state changing methods.
		for _, method := range contractDefinition.AssertionTestMethods {
			// Any non-constant method should be tracked as a state changing method.
			if method.IsConstant() {
				// Only track the pure/view method if testing view methods is enabled
				if fw.fuzzer.config.Fuzzing.Testing.AssertionTesting.TestViewMethods {
					fw.pureMethods = append(fw.pureMethods, fuzzerTypes.DeployedContractMethod{Address: contractAddress, Contract: contractDefinition, Method: method})
				}
			} else {
				fw.stateChangingMethods = append(fw.stateChangingMethods, fuzzerTypes.DeployedContractMethod{Address: contractAddress, Contract: contractDefinition, Method: method})
			}
		}
	}
}

// testNextCallSequence tests a call message sequence against the underlying FuzzerWorker's Chain and calls every
// CallSequenceTestFunc registered with the parent Fuzzer to update any test results. If any call message in the
// sequence is nil, a call message will be created in its place, targeting a state changing method of a contract
// deployed in the Chain.
// Returns the length of the call sequence tested, any requests for call sequence shrinking, or an error if one occurs.
func (fw *FuzzerWorker) testNextCallSequence() (calls.CallSequence, []ShrinkCallSequenceRequest, error) {
	// After testing the sequence, we'll want to rollback changes to reset our testing state.
	var err error
	defer func() {
		if err == nil {
			err = fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber)
		}
	}()

	// Initialize a new sequence within our sequence generator.
	var isNewSequence bool
	isNewSequence, err = fw.sequenceGenerator.InitializeNextSequence()
	if err != nil {
		return nil, nil, err
	}

	// Define our shrink requests we'll collect during execution.
	shrinkCallSequenceRequests := make([]ShrinkCallSequenceRequest, 0)

	// Our "fetch next call" method will generate new calls as needed, if we are generating a new sequence.
	fetchElementFunc := func(currentIndex int) (*calls.CallSequenceElement, error) {
		return fw.sequenceGenerator.PopSequenceElement()
	}

	// Our "post execution check function" method will check coverage and call all testing functions. If one returns a
	// request for a shrunk call sequence, we exit our call sequence execution immediately to go fulfill the shrink
	// request.
	executionCheckFunc := func(currentlyExecutedSequence calls.CallSequence) (bool, error) {
		// Check for updates to coverage and corpus.
		// If we detect coverage changes, add this sequence with weight as 1 + sequences tested (to avoid zero weights)
		err := fw.fuzzer.corpus.CheckSequenceCoverageAndUpdate(currentlyExecutedSequence, fw.getNewCorpusCallSequenceWeight(), true)
		if err != nil {
			return true, err
		}

		// Loop through each test function, signal our worker tested a call, and collect any requests to shrink
		// this call sequence.
		for _, callSequenceTestFunc := range fw.fuzzer.Hooks.CallSequenceTestFuncs {
			newShrinkRequests, err := callSequenceTestFunc(fw, currentlyExecutedSequence)
			if err != nil {
				return true, err
			}
			shrinkCallSequenceRequests = append(shrinkCallSequenceRequests, newShrinkRequests...)
		}

		// Update our metrics
		fw.workerMetrics().callsTested.Add(fw.workerMetrics().callsTested, big.NewInt(1))
		lastCallSequenceElement := currentlyExecutedSequence[len(currentlyExecutedSequence)-1]
		fw.workerMetrics().gasUsed.Add(fw.workerMetrics().gasUsed, new(big.Int).SetUint64(lastCallSequenceElement.ChainReference.Block.MessageResults[lastCallSequenceElement.ChainReference.TransactionIndex].Receipt.GasUsed))

		// If our fuzzer context is done, exit out immediately without results.
		if utils.CheckContextDone(fw.fuzzer.ctx) {
			return true, nil
		}

		// If we have shrink requests, it means we violated a test, so we quit at this point
		return len(shrinkCallSequenceRequests) > 0, nil
	}

	// Execute our call sequence.
	testedCallSequence, err := calls.ExecuteCallSequenceIteratively(fw.chain, fetchElementFunc, executionCheckFunc)

	// If we encountered an error, report it.
	if err != nil {
		return nil, nil, err
	}

	// If our fuzzer context is done, exit out immediately without results.
	if utils.CheckContextDone(fw.fuzzer.ctx) {
		return nil, nil, nil
	}

	// If this was not a new call sequence, indicate not to save the shrunken result to the corpus again.
	if !isNewSequence {
		for i := 0; i < len(shrinkCallSequenceRequests); i++ {
			shrinkCallSequenceRequests[i].RecordResultInCorpus = false
		}
	}

	// Return our results accordingly.
	return testedCallSequence, shrinkCallSequenceRequests, nil
}

// testShrunkenCallSequence tests a provided shrunken call sequence to verify it continues to satisfy the provided
// shrink verifier. Chain state is reverted to the testing base prior to returning.
// Returns a boolean indicating if the shrunken call sequence is valid for a given shrink request, or an error if one occurred.
func (fw *FuzzerWorker) testShrunkenCallSequence(possibleShrunkSequence calls.CallSequence, shrinkRequest ShrinkCallSequenceRequest) (bool, error) {
	// After testing the sequence, we'll want to rollback changes to reset our testing state.
	var err error
	defer func() {
		if err == nil {
			err = fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber)
		}
	}()

	// Our "fetch next call method" method will simply fetch and fix the call message in case any fields are not correct due to shrinking.
	fetchElementFunc := func(currentIndex int) (*calls.CallSequenceElement, error) {
		// If we are at the end of our sequence, return nil indicating we should stop executing.
		if currentIndex >= len(possibleShrunkSequence) {
			return nil, nil
		}

		possibleShrunkSequence[currentIndex].Call.FillFromTestChainProperties(fw.chain)
		return possibleShrunkSequence[currentIndex], nil
	}

	// Our "post-execution check" method will check coverage and call all testing functions. If one returns a
	// request for a shrunk call sequence, we exit our call sequence execution immediately to go fulfill the shrink
	// request.
	executionCheckFunc := func(currentlyExecutedSequence calls.CallSequence) (bool, error) {
		// Check for updates to coverage and corpus (using only the section of the sequence we tested so far).
		// If we detect coverage changes, add this sequence.
		seqErr := fw.fuzzer.corpus.CheckSequenceCoverageAndUpdate(currentlyExecutedSequence, fw.getNewCorpusCallSequenceWeight(), true)
		if seqErr != nil {
			return true, seqErr
		}

		// If our fuzzer context is done, exit out immediately without results.
		if utils.CheckContextDone(fw.fuzzer.ctx) {
			return true, nil
		}

		return false, nil
	}

	// Execute our call sequence.
	_, err = calls.ExecuteCallSequenceIteratively(fw.chain, fetchElementFunc, executionCheckFunc)
	if err != nil {
		return false, err
	}

	// If our fuzzer context is done, exit out immediately without results.
	if utils.CheckContextDone(fw.fuzzer.ctx) {
		return false, nil
	}

	// Check if our verifier signalled that we met our conditions
	validShrunkSequence := false
	if len(possibleShrunkSequence) > 0 {
		validShrunkSequence, err = shrinkRequest.VerifierFunction(fw, possibleShrunkSequence)
		if err != nil {
			return false, err
		}
	}
	return validShrunkSequence, nil
}

func (fw *FuzzerWorker) shrinkParam(callSequence *calls.CallSequence) {
	i := fw.randomProvider.Intn(len(*callSequence))
	abiValuesMsgData := (*callSequence)[i].Call.DataAbiValues
	for j := 0; j < len(abiValuesMsgData.InputValues); j++ {
		mutatedInput, _ := valuegeneration.MutateAbiValue(fw.sequenceGenerator.config.ValueGenerator, fw.shrinkingValueMutator, &abiValuesMsgData.Method.Inputs[j].Type, abiValuesMsgData.InputValues[j])
		abiValuesMsgData.InputValues[j] = mutatedInput
	}
	// Re-encode the message's calldata
	(*callSequence)[i].Call.WithDataAbiValues(abiValuesMsgData)
}

func (fw *FuzzerWorker) shorten(callSequence *calls.CallSequence) {
	i := fw.randomProvider.Intn(len(*callSequence))
	*callSequence = append((*callSequence)[:i], (*callSequence)[i+1:]...)
}

// shrinkCallSequence takes a provided call sequence and attempts to shrink it by looking for redundant
// calls which can be removed, and values which can be minimized, while continuing to satisfy the provided shrink
// verifier.
//
// This function should *always* be called if there are shrink requests, and should always report a result,
// even if it is the original sequence provided.
//
// Returns a call sequence that was optimized to include as little calls as possible to trigger the
// expected conditions, or an error if one occurred.
func (fw *FuzzerWorker) shrinkCallSequence(callSequence calls.CallSequence, shrinkRequest ShrinkCallSequenceRequest) (calls.CallSequence, error) {
	// Define a variable to track our most optimized sequence across all optimization iterations.
	optimizedSequence := callSequence

	// Obtain our shrink limits and begin shrinking.
	shrinkIteration := uint64(0)
	shrinkLimit := fw.fuzzer.config.Fuzzing.ShrinkLimit
	shrinkingEnded := func() bool {
		return shrinkIteration >= shrinkLimit || utils.CheckContextDone(fw.fuzzer.ctx)
	}
	if shrinkLimit > 0 {
		fw.workerMetrics().shrinking = true
		fw.fuzzer.logger.Info(fmt.Sprintf("[Worker %d] Shrinking call sequence with %d call(s)", fw.workerIndex, len(callSequence)))

		// First, remove all reverting txs from the sequence.
		var withoutReverts calls.CallSequence
		for i := 0; i < len(optimizedSequence) && !shrinkingEnded(); i++ {
			var err error
			withoutReverts, err = optimizedSequence.Clone()
			if err != nil {
				return nil, err
			}
			lastCall := withoutReverts[i]
			lastCallChainReference := lastCall.ChainReference
			lastMessageResult := lastCallChainReference.Block.MessageResults[lastCallChainReference.TransactionIndex]
			if lastMessageResult.Receipt.Status == types.ReceiptStatusFailed {
				withoutReverts = append(withoutReverts[:i], withoutReverts[i+1:]...)
			}
			shrinkLimit--
		}
		// Test the sequence with all reverts removed.
		validShrunkSequence, err := fw.testShrunkenCallSequence(withoutReverts, shrinkRequest)
		if err != nil {
			return nil, err
		}

		if validShrunkSequence {
			optimizedSequence = withoutReverts
		}

		for !shrinkingEnded() {

			// Clone the optimized sequence.
			possibleShrunkSequence, _ := optimizedSequence.Clone()

			// Alternate
			coinToss := fw.randomProvider.Int() % 2
			if coinToss == 0 || len(possibleShrunkSequence) == 1 {
				fw.shrinkParam(&possibleShrunkSequence)
			} else {
				fw.shorten(&possibleShrunkSequence)
			}

			// Test the shrunken sequence.
			validShrunkSequence, err := fw.testShrunkenCallSequence(possibleShrunkSequence, shrinkRequest)
			shrinkIteration++
			if err != nil {
				return nil, err
			}

			// If this current sequence satisfied our conditions, set it as our optimized sequence.
			if validShrunkSequence {
				optimizedSequence = possibleShrunkSequence
			}

			shrinkLimit--
		}
		fw.workerMetrics().shrinking = false
	}

	// If the shrink request wanted the sequence recorded in the corpus, do so now.
	if shrinkRequest.RecordResultInCorpus {
		err := fw.fuzzer.corpus.AddTestResultCallSequence(optimizedSequence, fw.getNewCorpusCallSequenceWeight(), true)
		if err != nil {
			return nil, err
		}
	}

	// Reset our state before running tracing in FinishedCallback.
	err := fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber)
	if err != nil {
		return nil, err
	}

	// Shrinking is complete. If our config specified we want all result sequences to have execution traces attached,
	// attach them now to each element in the sequence. Otherwise, call sequences will only have traces that the
	// test providers choose to attach themselves.
	err = shrinkRequest.FinishedCallback(fw, optimizedSequence, fw.fuzzer.config.Fuzzing.Testing.TraceAll)
	if err != nil {
		return nil, err
	}

	// After testing the sequence, we'll want to rollback changes to reset our testing state.
	if err = fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber); err != nil {
		return nil, err
	}
	return optimizedSequence, err
}

// run takes a base Chain in a setup state ready for testing, clones it, and begins executing fuzzed transaction calls
// and asserting properties are upheld. This runs until Fuzzer.ctx cancels the operation.
// Returns a boolean indicating whether Fuzzer.ctx has indicated we cancel the operation, and an error if one occurred.
func (fw *FuzzerWorker) run(baseTestChain *chain.TestChain) (bool, error) {
	// Clone our chain, attaching our necessary components for fuzzing post-genesis, prior to all blocks being copied.
	// This means any tracers added or events subscribed to within this inner function are done so prior to chain
	// setup (initial contract deployments), so data regarding that can be tracked as well.
	var err error
	fw.chain, err = baseTestChain.Clone(func(initializedChain *chain.TestChain) error {
		// Subscribe our chain event handlers
		initializedChain.Events.ContractDeploymentAddedEventEmitter.Subscribe(fw.onChainContractDeploymentAddedEvent)
		initializedChain.Events.ContractDeploymentRemovedEventEmitter.Subscribe(fw.onChainContractDeploymentRemovedEvent)

		// Emit an event indicating the worker has created its chain.
		err = fw.Events.FuzzerWorkerChainCreated.Publish(FuzzerWorkerChainCreatedEvent{
			Worker: fw,
			Chain:  initializedChain,
		})
		if err != nil {
			return fmt.Errorf("error returned by an event handler when emitting a worker chain created event: %v", err)
		}

		// If we have coverage-guided fuzzing enabled, create a tracer to collect coverage and connect it to the chain.
		if fw.fuzzer.config.Fuzzing.CoverageEnabled {
			fw.coverageTracer = coverage.NewCoverageTracer()
			initializedChain.AddTracer(fw.coverageTracer.NativeTracer(), true, false)
		}
		return nil
	})

	// If we encountered an error during cloning, return it.
	if err != nil {
		return false, err
	}

	// Defer the closing of the test chain object
	defer fw.chain.Close()

	// Emit an event indicating the worker has setup its chain.
	err = fw.Events.FuzzerWorkerChainSetup.Publish(FuzzerWorkerChainSetupEvent{
		Worker: fw,
		Chain:  fw.chain,
	})
	if err != nil {
		return false, fmt.Errorf("error returned by an event handler when emitting a worker chain setup event: %v", err)
	}

	// Increase our generation metric as we successfully generated a test node
	fw.workerMetrics().workerStartupCount.Add(fw.workerMetrics().workerStartupCount, big.NewInt(1))

	// Save the current block number as all contracts have been deployed at this point, and we'll want to revert
	// to this state between testing.
	fw.testingBaseBlockNumber = fw.chain.HeadBlockNumber()

	// Enter the main fuzzing loop, restricting our memory database size based on our config variable.
	// When the limit is reached, we exit this method gracefully, which will cause the fuzzing to recreate
	// this worker with a fresh memory database.
	sequencesTested := 0
	for sequencesTested <= fw.fuzzer.config.Fuzzing.WorkerResetLimit {
		// If our context signalled to close the operation, exit our testing loop accordingly, otherwise continue.
		if utils.CheckContextDone(fw.fuzzer.ctx) {
			return true, nil
		}

		// Emit an event indicating the worker is about to test a new call sequence.
		err := fw.Events.CallSequenceTesting.Publish(FuzzerWorkerCallSequenceTestingEvent{
			Worker: fw,
		})
		if err != nil {
			return false, fmt.Errorf("error returned by an event handler when a worker emitted an event indicating testing of a new call sequence is starting: %v", err)
		}

		// Test a new sequence
		callSequence, shrinkVerifiers, err := fw.testNextCallSequence()
		if err != nil {
			return false, err
		}

		// If we have any requests to shrink call sequences, do so now.
		for _, shrinkVerifier := range shrinkVerifiers {
			_, err = fw.shrinkCallSequence(callSequence, shrinkVerifier)
			if err != nil {
				return false, err
			}
		}

		// Emit an event indicating the worker is about to test a new call sequence.
		err = fw.Events.CallSequenceTested.Publish(FuzzerWorkerCallSequenceTestedEvent{
			Worker: fw,
		})
		if err != nil {
			return false, fmt.Errorf("error returned by an event handler when a worker emitted an event indicating testing of a new call sequence has concluded: %v", err)
		}

		// Update our sequences tested metrics
		fw.workerMetrics().sequencesTested.Add(fw.workerMetrics().sequencesTested, big.NewInt(1))
		sequencesTested++
	}

	// We have not cancelled fuzzing operations, but this worker exited, signalling for it to be regenerated.
	return false, nil
}
