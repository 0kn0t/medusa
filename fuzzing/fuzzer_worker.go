package fuzzing

import (
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/trailofbits/medusa/chain"
	"github.com/trailofbits/medusa/fuzzing/calls"
	fuzzerTypes "github.com/trailofbits/medusa/fuzzing/contracts"
	"github.com/trailofbits/medusa/fuzzing/coverage"
	"github.com/trailofbits/medusa/fuzzing/valuegeneration"
	"github.com/trailofbits/medusa/utils"
	"golang.org/x/exp/maps"
	"math/rand"
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

	// randomProvider provides random data as inputs to decisions throughout the worker.
	randomProvider *rand.Rand
	// sequenceGenerator creates entirely new or mutated call sequences based on corpus call sequences, for use in
	// fuzzing campaigns.
	sequenceGenerator *CallSequenceGenerator
	// valueSet defines a set derived from Fuzzer.BaseValueSet which is further populated with runtime values by the
	// FuzzerWorker. It is the value set shared with the underlying valueGenerator.
	valueSet *valuegeneration.ValueSet
	// valueGenerator generates values for use in the fuzzing campaign (e.g. when populating abi function call
	// arguments)
	valueGenerator valuegeneration.ValueGenerator

	// Events describes the event system for the FuzzerWorker.
	Events FuzzerWorkerEvents
}

// newFuzzerWorker creates a new FuzzerWorker, assigning it the provided worker index/id and associating it to the
// Fuzzer instance supplied.
// Returns the new FuzzerWorker
func newFuzzerWorker(fuzzer *Fuzzer, workerIndex int, randomProvider *rand.Rand) (*FuzzerWorker, error) {
	// Clone the fuzzer's base value set, so we can build on it with runtime values.
	valueSet := fuzzer.baseValueSet.Clone()

	// Create a value generator for the worker
	valueGenerator, err := fuzzer.Hooks.NewValueGeneratorFunc(fuzzer, valueSet, randomProvider)
	if err != nil {
		return nil, err
	}

	// Create a fuzzing worker struct, referencing our parent fuzzing.
	worker := &FuzzerWorker{
		workerIndex:          workerIndex,
		fuzzer:               fuzzer,
		deployedContracts:    make(map[common.Address]*fuzzerTypes.Contract),
		stateChangingMethods: make([]fuzzerTypes.DeployedContractMethod, 0),
		coverageTracer:       nil,
		randomProvider:       randomProvider,
		valueSet:             valueSet,
		valueGenerator:       valueGenerator,
	}
	worker.sequenceGenerator = NewCallSequenceGenerator(worker)

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
func (fw *FuzzerWorker) ValueGenerator() *valuegeneration.ValueSet {
	return fw.valueSet
}

// onChainContractDeploymentAddedEvent is the event callback used when the chain detects a new contract deployment.
// It attempts bytecode matching and updates the list of deployed contracts the worker should use for fuzz testing.
func (fw *FuzzerWorker) onChainContractDeploymentAddedEvent(event chain.ContractDeploymentsAddedEvent) error {
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

	// Update our state changing methods
	fw.updateStateChangingMethods()

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

	// Update our state changing methods
	fw.updateStateChangingMethods()

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

// updateStateChangingMethods updates the list of state changing methods used by the worker by re-evaluating them
// from the deployedContracts lookup.
func (fw *FuzzerWorker) updateStateChangingMethods() {
	// Clear our list of state changing methods
	fw.stateChangingMethods = make([]fuzzerTypes.DeployedContractMethod, 0)

	// Loop through each deployed contract
	for contractAddress, contractDefinition := range fw.deployedContracts {
		// If we deployed the contract, also enumerate property tests and state changing methods.
		for _, method := range contractDefinition.CompiledContract().Abi.Methods {
			if !method.IsConstant() {
				// Any non-constant method should be tracked as a state changing method.
				fw.stateChangingMethods = append(fw.stateChangingMethods, fuzzerTypes.DeployedContractMethod{Address: contractAddress, Contract: contractDefinition, Method: method})
			}
		}
	}
}

// testCallSequence tests a call message sequence against the underlying FuzzerWorker's Chain and calls every
// CallSequenceTestFunc registered with the parent Fuzzer to update any test results. If any call message in the
// sequence is nil, a call message will be created in its place, targeting a state changing method of a contract
// deployed in the Chain.
// Returns the length of the call sequence tested, any requests for call sequence shrinking, or an error if one occurs.
func (fw *FuzzerWorker) testCallSequence(callSequence calls.CallSequence) (int, []ShrinkCallSequenceRequest, error) {
	// After testing the sequence, we'll want to rollback changes to reset our testing state.
	defer func() {
		if err := fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber); err != nil {
			panic(err.Error())
		}
	}()

	// Request our sequence generator prepare for generation of our call sequence
	fw.sequenceGenerator.NewSequence(len(callSequence))

	// Define our shrink requests we'll collect during execution.
	shrinkCallSequenceRequests := make([]ShrinkCallSequenceRequest, 0)

	// Our pre-step method will generate new calls as needed, prior to them being executed.
	executePreStepFunc := func(i int) (bool, error) {
		// If our current call sequence element is nil, generate one.
		var err error
		if callSequence[i] == nil {
			callSequence[i], err = fw.sequenceGenerator.GenerateElement()
			if err != nil {
				return true, err
			}
		}
		return false, nil
	}

	// Our post-step method will check coverage and call all testing functions. If one returns a request for a shrunk
	// call sequence, we exit our call sequence execution immediately to go fulfill the shrink request.
	executePostStepFunc := func(i int) (bool, error) {
		// Slice off the currently tested part of our call sequence
		callSequenceTested := callSequence[:i+1]

		// Check for updates to coverage and corpus.
		err := fw.fuzzer.corpus.AddCallSequenceIfCoverageChanged(callSequenceTested)
		if err != nil {
			return true, err
		}

		// Loop through each test function, signal our worker tested a call, and collect any requests to shrink
		// this call sequence.
		for _, callSequenceTestFunc := range fw.fuzzer.Hooks.CallSequenceTestFuncs {
			newShrinkRequests, err := callSequenceTestFunc(fw, callSequenceTested)
			if err != nil {
				return true, err
			}
			shrinkCallSequenceRequests = append(shrinkCallSequenceRequests, newShrinkRequests...)
		}

		// Update our metrics
		fw.workerMetrics().callsTested++

		// If our fuzzer context is done, exit out immediately without results.
		if utils.CheckContextDone(fw.fuzzer.ctx) {
			return true, nil
		}

		// If we have shrink requests, it means we violated a test, so we quit at this point
		return len(shrinkCallSequenceRequests) > 0, nil
	}

	// Execute our call sequence.
	executedCount, err := callSequence.ExecuteOnChain(fw.chain, true, executePreStepFunc, executePostStepFunc)

	// Return our results accordingly.
	if err != nil {
		return executedCount, nil, err
	}

	// If our fuzzer context is done, exit out immediately without results.
	if utils.CheckContextDone(fw.fuzzer.ctx) {
		return executedCount, nil, nil
	}

	return executedCount, shrinkCallSequenceRequests, nil
}

// shrinkCallSequence takes a provided call sequence and attempts to shrink it by looking for redundant
// calls which can be removed that continue to satisfy the provided shrink verifier.
// Returns a call sequence that was optimized to include as little calls as possible to trigger the
// expected conditions, or an error if one occurred.
func (fw *FuzzerWorker) shrinkCallSequence(callSequence calls.CallSequence, shrinkRequest ShrinkCallSequenceRequest) (calls.CallSequence, error) {
	// In case of any error, we defer an operation to revert our chain state. We purposefully ignore errors from it to
	// prioritize any others which occurred.
	defer func() {
		// nolint:errcheck
		fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber)
	}()

	// Define another slice to store our tx sequence
	// Note: We clone here as we don't want to overwrite our call sequence runtime results we store.
	optimizedSequence := callSequence.Clone()

	for i := 0; i < len(optimizedSequence); {
		// Recreate our sequence without the item at this index
		possibleShrunkSequence := make(calls.CallSequence, 0)
		possibleShrunkSequence = append(possibleShrunkSequence, optimizedSequence[:i].Clone()...)
		possibleShrunkSequence = append(possibleShrunkSequence, optimizedSequence[i+1:].Clone()...)

		// Our pre-step method will simply correct the call message in case any fields are not correct due to shrinking.
		executePreStepFunc := func(currentIndex int) (bool, error) {
			possibleShrunkSequence[currentIndex].Call.FillFromTestChainProperties(fw.chain)
			return false, nil
		}

		// Our post-step method will check coverage and call all testing functions. If one returns a request for a shrunk
		// call sequence, we exit our call sequence execution immediately to go fulfill the shrink request.
		executePostStepFunc := func(currentIndex int) (bool, error) {
			// Check for updates to coverage and corpus (using only the section of the sequence we tested so far).
			err := fw.fuzzer.corpus.AddCallSequenceIfCoverageChanged(possibleShrunkSequence[:currentIndex+1])
			if err != nil {
				return true, err
			}

			// If our fuzzer context is done, exit out immediately without results.
			if utils.CheckContextDone(fw.fuzzer.ctx) {
				return true, nil
			}

			return false, nil
		}

		// Execute our call sequence.
		executedCount, err := possibleShrunkSequence.ExecuteOnChain(fw.chain, true, executePreStepFunc, executePostStepFunc)
		if err != nil {
			return nil, err
		}

		// If our fuzzer context is done, exit out immediately without results.
		if utils.CheckContextDone(fw.fuzzer.ctx) {
			return nil, nil
		}

		// Check if our verifier signalled that we met our conditions
		testedPossibleShrunkSequence := possibleShrunkSequence[:executedCount]
		validShrunkSequence := false
		if len(testedPossibleShrunkSequence) > 0 {
			validShrunkSequence, err = shrinkRequest.VerifierFunction(fw, testedPossibleShrunkSequence)
			if err != nil {
				return nil, err
			}
		}

		// After testing the sequence, we'll want to rollback changes to reset our testing state.
		if err = fw.chain.RevertToBlockNumber(fw.testingBaseBlockNumber); err != nil {
			return nil, err
		}

		// If this current sequence satisfied our conditions, set it as our optimized sequence.
		if validShrunkSequence {
			optimizedSequence = testedPossibleShrunkSequence
		} else {
			// We didn't remove an item at this index, so we'll iterate to the next one.
			i++
		}
	}

	// After we finished shrinking, report our result and return it.
	err := shrinkRequest.FinishedCallback(fw, optimizedSequence)
	if err != nil {
		return nil, err
	}

	return optimizedSequence, nil
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
			initializedChain.AddTracer(fw.coverageTracer, true, false)
		}
		return nil
	})

	// If we encountered an error during cloning, return it.
	if err != nil {
		return false, err
	}

	// Emit an event indicating the worker has setup its chain.
	err = fw.Events.FuzzerWorkerChainSetup.Publish(FuzzerWorkerChainSetupEvent{
		Worker: fw,
		Chain:  fw.chain,
	})
	if err != nil {
		return false, fmt.Errorf("error returned by an event handler when emitting a worker chain setup event: %v", err)
	}

	// Increase our generation metric as we successfully generated a test node
	fw.workerMetrics().workerStartupCount++

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

		// Define our call sequence slice to populate.
		callSequence := make(calls.CallSequence, fw.fuzzer.config.Fuzzing.CallSequenceLength)

		// Test a newly generated call sequence (nil entries are filled by the method during testing)
		txsTested, shrinkVerifiers, err := fw.testCallSequence(callSequence)
		if err != nil {
			return false, err
		}

		// If we have any requests to shrink call sequences, do so now.
		for _, shrinkVerifier := range shrinkVerifiers {
			_, err = fw.shrinkCallSequence(callSequence[:txsTested], shrinkVerifier)
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
		fw.workerMetrics().sequencesTested++
		sequencesTested++
	}

	// We have not cancelled fuzzing operations, but this worker exited, signalling for it to be regenerated.
	return false, nil
}
