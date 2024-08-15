package fuzzing

import (
	"fmt"
	"math/big"

	"github.com/crytic/medusa/fuzzing/calls"
	"github.com/crytic/medusa/fuzzing/contracts"
	"github.com/crytic/medusa/fuzzing/valuegeneration"
	"github.com/crytic/medusa/utils"
	"github.com/crytic/medusa/utils/randomutils"
)

// CallSequenceGenerator generates call sequences iteratively per element, for use in fuzzing campaigns. It is attached
// to a FuzzerWorker and uses its runtime context
type CallSequenceGenerator struct {
	// worker describes the parent FuzzerWorker using this mutator. Calls will be generated against deployed contract
	// methods known to the worker.
	worker *FuzzerWorker

	// config describes the weights to use for each weighted random CallSequenceGeneratorMutationStrategy this
	// CallSequenceGenerator will use when generating new call sequences.
	config *CallSequenceGeneratorConfig

	// baseSequence describes the internal call sequence generated by InitializeNextSequence to use as a base when providing
	// potentially further mutated values with PopSequenceElement iteratively.
	baseSequence calls.CallSequence

	// fetchIndex describes the current position in the baseSequence which defines the next element to be mutated and
	// returned when calling PopSequenceElement.
	fetchIndex int

	// prefetchModifyCallFunc describes the method to use to mutate the next indexed call sequence element, prior
	// to its fetching by PopSequenceElement.
	prefetchModifyCallFunc PrefetchModifyCallFunc

	// mutationStrategyChooser is a weighted random selector of functions that prepare the CallSequenceGenerator with
	// a baseSequence derived from corpus entries.
	mutationStrategyChooser *randomutils.WeightedRandomChooser[CallSequenceGeneratorMutationStrategy]
}

// CallSequenceGeneratorConfig defines the configuration for a CallSequenceGenerator to be created and used by a
// FuzzerWorker to generate call sequences in a fuzzing campaign.
type CallSequenceGeneratorConfig struct {
	// NewSequenceProbability defines the probability that the CallSequenceGenerator should generate an entirely new
	// sequence rather than mutating one from the corpus.
	NewSequenceProbability float32

	// RandomUnmodifiedCorpusHeadWeight defines the weight that the CallSequenceGenerator should use the call sequence
	// generation strategy of taking the head of a corpus sequence (without mutations) and append newly generated calls
	// to the end of it.
	RandomUnmodifiedCorpusHeadWeight uint64

	// RandomUnmodifiedCorpusTailWeight defines the weight that the CallSequenceGenerator should use the call sequence
	// generation strategy of taking the tail of a corpus sequence (without mutations) and prepend newly generated calls
	// to the start of it.
	RandomUnmodifiedCorpusTailWeight uint64

	// RandomUnmodifiedSpliceAtRandomWeight defines the weight that the CallSequenceGenerator should use the call sequence
	// generation strategy of taking two corpus sequences (without mutations) and splicing them before joining them
	// together.
	RandomUnmodifiedSpliceAtRandomWeight uint64

	// RandomUnmodifiedInterleaveAtRandomWeight defines the weight that the CallSequenceGenerator should use the call
	// sequence generation strategy of taking two corpus sequences (without mutations) and interleaving a random
	// number of calls from each.
	RandomUnmodifiedInterleaveAtRandomWeight uint64

	// RandomMutatedCorpusHeadWeight defines the weight that the CallSequenceGenerator should use the call sequence
	// generation strategy of taking the head of a corpus sequence (with mutations) and append newly generated calls
	// to the end of it.
	RandomMutatedCorpusHeadWeight uint64

	// RandomMutatedCorpusTailWeight defines the weight that the CallSequenceGenerator should use the call sequence
	// generation strategy of taking the tao; of a corpus sequence (with mutations) and prepend newly generated calls
	// to the start of it.
	RandomMutatedCorpusTailWeight uint64

	// RandomMutatedSpliceAtRandomWeight defines the weight that the CallSequenceGenerator should use the call sequence
	// generation strategy of taking two corpus sequences (with mutations) and splicing them before joining them
	// together.
	RandomMutatedSpliceAtRandomWeight uint64

	// RandomMutatedInterleaveAtRandomWeight defines the weight that the CallSequenceGenerator should use the call
	// sequence generation strategy of taking two corpus sequences (with mutations) and interleaving a random
	// number of calls from each.
	RandomMutatedInterleaveAtRandomWeight uint64

	// ValueGenerator defines the value provider to use when generating new values for call sequences. This is used both
	// for ABI call data generation, and generation of additional values such as the "value" field of a
	// transaction/call.
	ValueGenerator valuegeneration.ValueGenerator

	// ValueMutator defines the value provider to use when mutating corpus call sequences.
	ValueMutator valuegeneration.ValueMutator
}

// CallSequenceGeneratorFunc defines a method used to populate a provided call sequence with generated calls.
// Returns an optional PrefetchModifyCallFunc to be executed prior to the fetching of each element, or an error if
// one occurs.
type CallSequenceGeneratorFunc func(sequenceGenerator *CallSequenceGenerator, sequence calls.CallSequence) error

// PrefetchModifyCallFunc defines a method used to modify a call sequence element before being fetched from this
// provider for use.
// Returns an error if one occurs.
type PrefetchModifyCallFunc func(sequenceGenerator *CallSequenceGenerator, element *calls.CallSequenceElement) error

// CallSequenceGeneratorMutationStrategy defines a structure for a mutation strategy used by a CallSequenceGenerator.
type CallSequenceGeneratorMutationStrategy struct {
	// CallSequenceGeneratorFunc describes a method used to populate a provided call sequence.
	CallSequenceGeneratorFunc CallSequenceGeneratorFunc

	// PrefetchModifyCallFunc defines a method used to modify a call sequence element before being fetched.
	PrefetchModifyCallFunc PrefetchModifyCallFunc
}

// NewCallSequenceGenerator creates a CallSequenceGenerator to generate call sequences for use in fuzzing campaigns.
func NewCallSequenceGenerator(worker *FuzzerWorker, config *CallSequenceGeneratorConfig) *CallSequenceGenerator {
	generator := &CallSequenceGenerator{
		worker:                  worker,
		config:                  config,
		mutationStrategyChooser: randomutils.NewWeightedRandomChooser[CallSequenceGeneratorMutationStrategy](),
	}

	generator.mutationStrategyChooser.AddChoices(
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncCorpusHead,
				PrefetchModifyCallFunc:    nil,
			},
			new(big.Int).SetUint64(config.RandomUnmodifiedCorpusHeadWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncCorpusTail,
				PrefetchModifyCallFunc:    nil,
			},
			new(big.Int).SetUint64(config.RandomUnmodifiedCorpusTailWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncExpansion,
				PrefetchModifyCallFunc:    nil,
			},
			new(big.Int).SetUint64(config.RandomUnmodifiedCorpusTailWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncSpliceAtRandom,
				PrefetchModifyCallFunc:    nil,
			},
			new(big.Int).SetUint64(config.RandomUnmodifiedSpliceAtRandomWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncInterleaveAtRandom,
				PrefetchModifyCallFunc:    nil,
			},
			new(big.Int).SetUint64(config.RandomUnmodifiedInterleaveAtRandomWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncCorpusHead,
				PrefetchModifyCallFunc:    prefetchModifyCallFuncMutate,
			},
			new(big.Int).SetUint64(config.RandomMutatedCorpusHeadWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncCorpusTail,
				PrefetchModifyCallFunc:    prefetchModifyCallFuncMutate,
			},
			new(big.Int).SetUint64(config.RandomMutatedCorpusTailWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncSpliceAtRandom,
				PrefetchModifyCallFunc:    prefetchModifyCallFuncMutate,
			},
			new(big.Int).SetUint64(config.RandomMutatedSpliceAtRandomWeight),
		),
		randomutils.NewWeightedRandomChoice(
			CallSequenceGeneratorMutationStrategy{
				CallSequenceGeneratorFunc: callSeqGenFuncInterleaveAtRandom,
				PrefetchModifyCallFunc:    prefetchModifyCallFuncMutate,
			},
			new(big.Int).SetUint64(config.RandomMutatedInterleaveAtRandomWeight),
		),
	)

	return generator
}

// InitializeNextSequence prepares the CallSequenceGenerator for generating a new sequence. Each element can be
// obtained by calling PopSequenceElement iteratively.
// Returns a boolean indicating whether the initialized sequence is a newly generated sequence (rather than an
// unmodified one loaded from the corpus), or an error if one occurred.
func (g *CallSequenceGenerator) InitializeNextSequence() (bool, error) {
	// Reset the state of our generator.
	g.baseSequence = make(calls.CallSequence, g.worker.fuzzer.config.Fuzzing.CallSequenceLength)
	g.fetchIndex = 0
	g.prefetchModifyCallFunc = nil

	// Check if there are any previously un-executed corpus call sequences. If there are, the fuzzer should execute
	// those first.
	unexecutedSequence := g.worker.fuzzer.corpus.UnexecutedCallSequence()
	if unexecutedSequence != nil {
		g.baseSequence = *unexecutedSequence
		return false, nil
	}

	// We'll decide whether to create a new call sequence or mutating existing corpus call sequences. Any entries we
	// leave as nil will be populated by a newly generated call prior to being fetched from this provider.

	// If this provider has no corpus mutation methods or corpus call sequences, we return a call sequence with
	// nil elements to signal that we want an entirely new sequence.
	if g.mutationStrategyChooser.ChoiceCount() == 0 || g.worker.fuzzer.corpus.ActiveMutableSequenceCount() == 0 {
		return true, nil
	}

	// Determine whether we will generate a corpus based mutated sequence.
	if g.worker.randomProvider.Float32() > g.config.NewSequenceProbability {
		// Get a random mutator function.
		corpusMutationFunc, err := g.mutationStrategyChooser.Choose()
		if err != nil {
			return true, fmt.Errorf("could not generate a corpus mutation derived call sequence due to an error obtaining a mutation method: %v", err)
		}

		// If we have a corpus mutation method, call it to generate our base sequence, then set the pre-fetch modify
		// call function.
		if corpusMutationFunc != nil && corpusMutationFunc.CallSequenceGeneratorFunc != nil {
			err = corpusMutationFunc.CallSequenceGeneratorFunc(g, g.baseSequence)
			if err != nil {
				return true, fmt.Errorf("could not generate a corpus mutation derived call sequence due to an error executing a mutation method: %v", err)
			}
			g.prefetchModifyCallFunc = corpusMutationFunc.PrefetchModifyCallFunc
		}
	}
	return true, nil
}

// PopSequenceElement obtains the next element for our call sequence requested by InitializeNextSequence. If there are no elements
// left to return, this method returns nil. If an error occurs, it is returned instead.
func (g *CallSequenceGenerator) PopSequenceElement() (*calls.CallSequenceElement, error) {
	// If the call sequence length is zero, there is no work to be done.
	if g.fetchIndex >= len(g.baseSequence) {
		return nil, nil
	}

	// Obtain our base call element
	element := g.baseSequence[g.fetchIndex]

	// If it is nil, we generate an entirely new call. Otherwise, we apply pre-execution modifications.
	var err error
	if element == nil {
		element, err = g.generateNewElement()
		if err != nil {
			return nil, err
		}
	} else {
		// We have an element, if our generator set a post-call modify for this function, execute it now to modify
		// our call prior to return. This allows mutations to be applied on a per-call time frame, rather than
		// per-sequence, making use of the most recent runtime data.
		if g.prefetchModifyCallFunc != nil {
			err = g.prefetchModifyCallFunc(g, element)
			if err != nil {
				return nil, err
			}
		}
	}

	// Update the element with the current nonce for the associated chain.
	element.Call.FillFromTestChainProperties(g.worker.chain)

	// Update our base sequence, advance our position, and return the processed element from this round.
	g.baseSequence[g.fetchIndex] = element
	g.fetchIndex++
	return element, nil
}

// generateNewElement generates a new call sequence element which targets a method in a contract
// deployed to the CallSequenceGenerator's parent FuzzerWorker chain, with fuzzed call data.
// Returns the call sequence element, or an error if one was encountered.
func (g *CallSequenceGenerator) generateNewElement() (*calls.CallSequenceElement, error) {
	// Check to make sure that we have any functions to call
	if len(g.worker.stateChangingMethods) == 0 && len(g.worker.pureMethods) == 0 {
		return nil, fmt.Errorf("cannot generate fuzzed call as there are no methods to call")
	}

	// Only call view functions if there are no state-changing methods
	var callOnlyPureFunctions bool
	if len(g.worker.stateChangingMethods) == 0 && len(g.worker.pureMethods) > 0 {
		callOnlyPureFunctions = true
	}

	// Select a random method
	// There is a 1/100 chance that a pure method will be invoked or if there are only pure functions that are callable
	var selectedMethod *contracts.DeployedContractMethod
	if (len(g.worker.pureMethods) > 0 && g.worker.randomProvider.Intn(100) == 0) || callOnlyPureFunctions {
		selectedMethod = &g.worker.pureMethods[g.worker.randomProvider.Intn(len(g.worker.pureMethods))]
	} else {
		selectedMethod = &g.worker.stateChangingMethods[g.worker.randomProvider.Intn(len(g.worker.stateChangingMethods))]
	}

	// Select a random sender
	selectedSender := g.worker.fuzzer.senders[g.worker.randomProvider.Intn(len(g.worker.fuzzer.senders))]

	// Generate fuzzed parameters for the function call
	args := make([]any, len(selectedMethod.Method.Inputs))
	for i := 0; i < len(args); i++ {
		// Create our fuzzed parameters.
		input := selectedMethod.Method.Inputs[i]
		args[i] = valuegeneration.GenerateAbiValue(g.config.ValueGenerator, &input.Type)
	}

	// If this is a payable function, generate value to send
	var value *big.Int
	value = big.NewInt(0)
	if selectedMethod.Method.StateMutability == "payable" {
		value = g.config.ValueGenerator.GenerateInteger(false, 64)
	}

	// Create our message using the provided parameters.
	// We fill out some fields and populate the rest from our TestChain properties.
	// TODO: We likely want to make gasPrice fluctuate within some sensible range here.
	msg := calls.NewCallMessageWithAbiValueData(selectedSender, &selectedMethod.Address, 0, value, g.worker.fuzzer.config.Fuzzing.TransactionGasLimit, nil, nil, nil, &calls.CallMessageDataAbiValues{
		Method:      &selectedMethod.Method,
		InputValues: args,
	})

	// Determine our delay values for this element
	blockNumberDelay := uint64(0)
	blockTimestampDelay := uint64(0)
	if g.worker.fuzzer.config.Fuzzing.MaxBlockNumberDelay > 0 {
		blockNumberDelay = g.config.ValueGenerator.GenerateInteger(false, 64).Uint64() % (g.worker.fuzzer.config.Fuzzing.MaxBlockNumberDelay + 1)
	}
	if g.worker.fuzzer.config.Fuzzing.MaxBlockTimestampDelay > 0 {
		blockTimestampDelay = g.config.ValueGenerator.GenerateInteger(false, 64).Uint64() % (g.worker.fuzzer.config.Fuzzing.MaxBlockTimestampDelay + 1)
	}

	// For each block we jump, we need a unique time stamp for chain semantics, so if our block number jump is too small,
	// while our timestamp jump is larger, we cap it.
	if blockNumberDelay > blockTimestampDelay {
		if blockTimestampDelay == 0 {
			blockNumberDelay = 0
		} else {
			blockNumberDelay %= blockTimestampDelay
		}
	}

	// Return our call sequence element.
	return calls.NewCallSequenceElement(selectedMethod.Contract, msg, blockNumberDelay, blockTimestampDelay), nil
}

// callSeqGenFuncCorpusHead is a CallSequenceGeneratorFunc which prepares a CallSequenceGenerator to generate a sequence
// whose head is based off of an existing corpus call sequence.
// Returns an error if one occurs.
func callSeqGenFuncCorpusHead(sequenceGenerator *CallSequenceGenerator, sequence calls.CallSequence) error {
	// Obtain a call sequence from the corpus
	corpusSequence, err := sequenceGenerator.worker.fuzzer.corpus.RandomMutationTargetSequence()
	if err != nil {
		return fmt.Errorf("could not obtain corpus call sequence for head mutation: %v", err)
	}

	// Determine the length of the slice to be copied in the head.
	maxLength := utils.Min(len(sequence), len(corpusSequence))
	copy(sequence, corpusSequence[:maxLength])

	return nil
}

// callSeqGenFuncCorpusTail is a CallSequenceGeneratorFunc which prepares a CallSequenceGenerator to generate a sequence
// whose tail is based off of an existing corpus call sequence.
// Returns an error if one occurs.
func callSeqGenFuncCorpusTail(sequenceGenerator *CallSequenceGenerator, sequence calls.CallSequence) error {
	// Obtain a call sequence from the corpus
	corpusSequence, err := sequenceGenerator.worker.fuzzer.corpus.RandomMutationTargetSequence()
	if err != nil {
		return fmt.Errorf("could not obtain corpus call sequence for tail mutation: %v", err)
	}

	// Determine a random position to slice the call sequence.
	maxLength := utils.Min(len(sequence), len(corpusSequence))
	targetLength := sequenceGenerator.worker.randomProvider.Intn(maxLength) + 1
	copy(sequence[len(sequence)-targetLength:], corpusSequence[len(corpusSequence)-targetLength:])

	return nil
}

// callSeqGenFuncExpansion is a CallSequenceGeneratorFunc which prepares a CallSequenceGenerator to generate a
// sequence which is expanded up to 30 times by replicating an existing call sequence element at a random position.
func callSeqGenFuncExpansion(sequenceGenerator *CallSequenceGenerator, sequence calls.CallSequence) error {
	rounds := sequenceGenerator.worker.randomProvider.Intn(31)

	// Get item to expand
	randIndex := sequenceGenerator.worker.randomProvider.Intn(len(sequence))
	duplicatedElement := sequence[randIndex]

	// Perform N rounds of expansion
	for i := 0; i < rounds; i++ {
		randIndex += i
		if randIndex < len(sequence) {
			// Insert
			sequence = append(sequence[:randIndex], append([]*calls.CallSequenceElement{duplicatedElement}, sequence[randIndex:]...)...)
		} else {
			// Extend
			sequence = append(sequence, duplicatedElement)
		}
	}
	return nil
}

// callSeqGenFuncSpliceAtRandom is a CallSequenceGeneratorFunc which prepares a CallSequenceGenerator to generate a
// sequence which is based off of two corpus call sequence entries, from which a random length head and tail are
// respectively sliced and joined together.
// Returns an error if one occurs.
func callSeqGenFuncSpliceAtRandom(sequenceGenerator *CallSequenceGenerator, sequence calls.CallSequence) error {
	// Obtain two corpus call sequence entries
	headSequence, err := sequenceGenerator.worker.fuzzer.corpus.RandomMutationTargetSequence()
	if err != nil {
		return fmt.Errorf("could not obtain head corpus call sequence for splice-at-random corpus mutation: %v", err)
	}
	tailSequence, err := sequenceGenerator.worker.fuzzer.corpus.RandomMutationTargetSequence()
	if err != nil {
		return fmt.Errorf("could not obtain tail corpus call sequence for splice-at-random corpus mutation: %v", err)
	}

	// Determine a random position to slice off the head of the call sequence.
	maxLength := utils.Min(len(sequence), len(headSequence))
	headSequenceLength := sequenceGenerator.worker.randomProvider.Intn(maxLength) + 1

	// Copy the head of the first corpus sequence to our destination sequence.
	copy(sequence, headSequence[:headSequenceLength])

	// Determine a random position to slice off the tail of the call sequence.
	maxLength = utils.Min(len(sequence)-headSequenceLength, len(tailSequence))
	tailSequenceLength := sequenceGenerator.worker.randomProvider.Intn(maxLength + 1)

	// Copy the tail of the second corpus sequence to our destination sequence (after the head sequence portion).
	copy(sequence[headSequenceLength:], tailSequence[len(tailSequence)-tailSequenceLength:])

	return nil
}

// callSeqGenFuncInterleaveAtRandom is a CallSequenceGeneratorFunc which prepares a CallSequenceGenerator to generate a
// sequence which is based off of two corpus call sequence entries, from which a random number of transactions are
// taken and interleaved (each element of one sequence will be followed by an element of the other).
// Returns an error if one occurs.
func callSeqGenFuncInterleaveAtRandom(sequenceGenerator *CallSequenceGenerator, sequence calls.CallSequence) error {
	// Obtain two corpus call sequence entries
	firstSequence, err := sequenceGenerator.worker.fuzzer.corpus.RandomMutationTargetSequence()
	if err != nil {
		return fmt.Errorf("could not obtain first corpus call sequence for interleave-at-random corpus mutation: %v", err)
	}
	secondSequence, err := sequenceGenerator.worker.fuzzer.corpus.RandomMutationTargetSequence()
	if err != nil {
		return fmt.Errorf("could not obtain second corpus call sequence for interleave-at-random corpus mutation: %v", err)
	}

	// Determine how many transactions to take from the first sequence and slice it.
	maxLength := utils.Min(len(sequence), len(firstSequence))
	firstSequenceLength := sequenceGenerator.worker.randomProvider.Intn(maxLength) + 1
	firstSequence = firstSequence[:firstSequenceLength]

	// Determine how many transactions to take from the second sequence and slice it.
	maxLength = utils.Min(len(sequence)-firstSequenceLength, len(secondSequence))
	secondSequenceLength := sequenceGenerator.worker.randomProvider.Intn(maxLength + 1)
	secondSequence = secondSequence[:secondSequenceLength]

	// Now that we have both sequences, and we know they will not exceed our destination sequence length, interleave
	// them.
	destIndex := 0
	largestSequenceSize := utils.Max(firstSequenceLength, secondSequenceLength)
	for i := 0; i < largestSequenceSize; i++ {
		if i < len(firstSequence) {
			sequence[destIndex] = firstSequence[i]
			destIndex++
		}
		if i < len(secondSequence) {
			sequence[destIndex] = secondSequence[i]
			destIndex++
		}
	}
	return nil
}

// prefetchModifyCallFuncMutate is a PrefetchModifyCallFunc, called by a CallSequenceGenerator to apply mutations
// to a call sequence element, prior to it being fetched.
// Returns an error if one occurs.
func prefetchModifyCallFuncMutate(sequenceGenerator *CallSequenceGenerator, element *calls.CallSequenceElement) error {
	// If this element has no ABI value based call data, exit early.
	if element.Call == nil || element.Call.DataAbiValues == nil {
		return nil
	}

	// Loop for each input value and mutate it
	abiValuesMsgData := element.Call.DataAbiValues
	for i := 0; i < len(abiValuesMsgData.InputValues); i++ {
		mutatedInput, err := valuegeneration.MutateAbiValue(sequenceGenerator.config.ValueGenerator, sequenceGenerator.config.ValueMutator, &abiValuesMsgData.Method.Inputs[i].Type, abiValuesMsgData.InputValues[i])
		if err != nil {
			return fmt.Errorf("error when mutating call sequence input argument: %v", err)
		}
		abiValuesMsgData.InputValues[i] = mutatedInput
	}
	// Re-encode the message's calldata
	element.Call.WithDataAbiValues(abiValuesMsgData)

	return nil
}
