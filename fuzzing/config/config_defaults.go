package config

import (
	"github.com/rs/zerolog"
	testChainConfig "github.com/trailofbits/medusa/chain/config"
	"github.com/trailofbits/medusa/compilation"
)

// GetDefaultProjectConfig obtains a default configuration for a project. It populates a default compilation config
// based on the provided platform, or a nil one if an empty string is provided.
func GetDefaultProjectConfig(platform string) (*ProjectConfig, error) {
	var (
		compilationConfig *compilation.CompilationConfig
		chainConfig       *testChainConfig.TestChainConfig
		err               error
	)

	// Try to obtain a default compilation config for this platform.
	if platform != "" {
		compilationConfig, err = compilation.NewCompilationConfig(platform)
		if err != nil {
			return nil, err
		}
	}

	// Try to obtain a default chain config.
	chainConfig, err = testChainConfig.DefaultTestChainConfig()
	if err != nil {
		return nil, err
	}

	// Create a project configuration
	projectConfig := &ProjectConfig{
		Fuzzing: FuzzingConfig{
			Workers:            10,
			WorkerResetLimit:   50,
			Timeout:            0,
			TestLimit:          0,
			CallSequenceLength: 100,
			DeploymentOrder:    []string{},
			ConstructorArgs:    map[string]map[string]any{},
			CorpusDirectory:    "",
			CoverageEnabled:    true,
			SenderAddresses: []string{
				"0x1111111111111111111111111111111111111111",
				"0x2222222222222222222222222222222222222222",
				"0x3333333333333333333333333333333333333333",
			},
			DeployerAddress:        "0x1111111111111111111111111111111111111111",
			MaxBlockNumberDelay:    60480,
			MaxBlockTimestampDelay: 604800,
			BlockGasLimit:          125_000_000,
			TransactionGasLimit:    12_500_000,
			Testing: TestingConfig{
				StopOnFailedTest:             true,
				StopOnFailedContractMatching: true,
				AssertionTesting: AssertionTestingConfig{
					Enabled:         false,
					TestViewMethods: false,
				},
				PropertyTesting: PropertyTestConfig{
					Enabled: true,
					TestPrefixes: []string{
						"fuzz_",
					},
				},
			},
			TestChain: *chainConfig,
		},
		Compilation: compilationConfig,
		Logging: LoggingConfig{
			Level:                zerolog.DebugLevel,
			EnableConsoleLogging: true,
			LogDirectory:         "logs",
		},
	}

	// Return the project configuration
	return projectConfig, nil
}
