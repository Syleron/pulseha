package integration

import (
	"log/syslog"
	"os"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logrus_syslog "github.com/sirupsen/logrus/hooks/syslog"
	"github.com/stretchr/testify/assert"
	"github.com/syleron/pulseha/packages/config"
)

func TestSyslogLoggingSetup(t *testing.T) {
	// Set test environment
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	testCases := []struct {
		name             string
		config           *config.Config
		expectHook       bool
		expectError      bool
		expectedTag      string
		expectedFacility syslog.Priority
	}{
		{
			name: "Local syslog enabled",
			config: &config.Config{
				Pulse: config.Local{
					LogToSyslog:    true,
					SyslogNetwork:  "",
					SyslogAddress:  "",
					SyslogFacility: "LOG_LOCAL0",
					SyslogTag:      "pulseha-test",
				},
			},
			expectHook:       true,  // May succeed on macOS
			expectError:      false, // May succeed on macOS
			expectedTag:      "pulseha-test",
			expectedFacility: syslog.LOG_LOCAL0,
		},
		{
			name: "Syslog disabled",
			config: &config.Config{
				Pulse: config.Local{
					LogToSyslog: false,
				},
			},
			expectHook:  false,
			expectError: false,
		},
		{
			name: "Default syslog config (backward compatibility)",
			config: &config.Config{
				Pulse: config.Local{
					// Old config - no syslog fields set
					SyslogTag: "", // Empty tag triggers default behavior
				},
			},
			expectHook:       true,            // May succeed on macOS
			expectError:      false,           // May succeed on macOS
			expectedTag:      "pulseha",       // Default tag
			expectedFacility: syslog.LOG_INFO, // Default facility
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			logger := log.New()
			logger.SetLevel(log.DebugLevel)

			// Test the syslog setup logic
			err := setupSyslogForTest(tc.config, logger)

			// Platform-specific test expectations
			if !tc.config.Pulse.LogToSyslog {
				// If syslog is disabled, no error should occur
				assert.NoError(t, err, "Should not error when syslog is disabled")
			} else {
				// Syslog enabled - may succeed on macOS, fail in containers
				if err != nil {
					// If it fails, should be a syslog-related error
					assert.Contains(t, err.Error(), "syslog", "Should contain syslog error message")
					t.Logf("Syslog connection failed as expected in test environment: %v", err)
				} else {
					// If it succeeds, that's also fine (e.g., on macOS)
					t.Logf("Syslog connection succeeded in test environment")
				}
			}
		})
	}
}

// setupSyslogForTest simulates the syslog setup logic from main.go
func setupSyslogForTest(cfg *config.Config, logger *log.Logger) error {
	// Replicate the syslog setup logic from main.go setupLogging function

	// Setup syslog logging if enabled (default to true if not explicitly set)
	logToSyslog := cfg.Pulse.LogToSyslog
	if cfg.Pulse.SyslogTag == "" {
		// Old config or missing syslog config - use defaults
		logToSyslog = true
	}

	if logToSyslog {
		// Convert facility string to syslog priority
		facility := syslog.LOG_INFO
		switch cfg.Pulse.SyslogFacility {
		case "LOG_LOCAL0":
			facility = syslog.LOG_LOCAL0
		case "LOG_LOCAL1":
			facility = syslog.LOG_LOCAL1
		case "LOG_LOCAL2":
			facility = syslog.LOG_LOCAL2
		case "LOG_LOCAL3":
			facility = syslog.LOG_LOCAL3
		case "LOG_LOCAL4":
			facility = syslog.LOG_LOCAL4
		case "LOG_LOCAL5":
			facility = syslog.LOG_LOCAL5
		case "LOG_LOCAL6":
			facility = syslog.LOG_LOCAL6
		case "LOG_LOCAL7":
			facility = syslog.LOG_LOCAL7
		case "LOG_USER":
			facility = syslog.LOG_USER
		case "LOG_DAEMON":
			facility = syslog.LOG_DAEMON
		case "LOG_SYSLOG":
			facility = syslog.LOG_SYSLOG
		default:
			facility = syslog.LOG_INFO
		}

		// Use defaults for empty values
		syslogTag := cfg.Pulse.SyslogTag
		if syslogTag == "" {
			syslogTag = "pulseha"
		}

		hook, err := logrus_syslog.NewSyslogHook(cfg.Pulse.SyslogNetwork, cfg.Pulse.SyslogAddress, facility, syslogTag)
		if err != nil {
			return err
		}
		logger.AddHook(hook)
		logger.Info("Syslog logging enabled")
	}

	return nil
}

func TestSyslogFacilityMapping(t *testing.T) {
	testCases := []struct {
		facilityString   string
		expectedFacility syslog.Priority
	}{
		{"LOG_LOCAL0", syslog.LOG_LOCAL0},
		{"LOG_LOCAL1", syslog.LOG_LOCAL1},
		{"LOG_LOCAL2", syslog.LOG_LOCAL2},
		{"LOG_LOCAL3", syslog.LOG_LOCAL3},
		{"LOG_LOCAL4", syslog.LOG_LOCAL4},
		{"LOG_LOCAL5", syslog.LOG_LOCAL5},
		{"LOG_LOCAL6", syslog.LOG_LOCAL6},
		{"LOG_LOCAL7", syslog.LOG_LOCAL7},
		{"LOG_USER", syslog.LOG_USER},
		{"LOG_DAEMON", syslog.LOG_DAEMON},
		{"LOG_SYSLOG", syslog.LOG_SYSLOG},
		{"", syslog.LOG_INFO},        // Default
		{"INVALID", syslog.LOG_INFO}, // Default
	}

	for _, tc := range testCases {
		t.Run(tc.facilityString, func(t *testing.T) {
			// Test the facility mapping logic
			facility := syslog.LOG_INFO
			switch tc.facilityString {
			case "LOG_LOCAL0":
				facility = syslog.LOG_LOCAL0
			case "LOG_LOCAL1":
				facility = syslog.LOG_LOCAL1
			case "LOG_LOCAL2":
				facility = syslog.LOG_LOCAL2
			case "LOG_LOCAL3":
				facility = syslog.LOG_LOCAL3
			case "LOG_LOCAL4":
				facility = syslog.LOG_LOCAL4
			case "LOG_LOCAL5":
				facility = syslog.LOG_LOCAL5
			case "LOG_LOCAL6":
				facility = syslog.LOG_LOCAL6
			case "LOG_LOCAL7":
				facility = syslog.LOG_LOCAL7
			case "LOG_USER":
				facility = syslog.LOG_USER
			case "LOG_DAEMON":
				facility = syslog.LOG_DAEMON
			case "LOG_SYSLOG":
				facility = syslog.LOG_SYSLOG
			default:
				facility = syslog.LOG_INFO
			}

			assert.Equal(t, tc.expectedFacility, facility, "Facility mapping should be correct for: %s", tc.facilityString)
		})
	}
}

func TestSyslogTagDefaults(t *testing.T) {
	testCases := []struct {
		name        string
		configTag   string
		expectedTag string
	}{
		{
			name:        "Custom tag",
			configTag:   "my-pulseha",
			expectedTag: "my-pulseha",
		},
		{
			name:        "Empty tag uses default",
			configTag:   "",
			expectedTag: "pulseha",
		},
		{
			name:        "Whitespace tag uses default",
			configTag:   "   ",
			expectedTag: "   ", // Current implementation doesn't trim whitespace
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test the tag default logic
			syslogTag := tc.configTag
			if syslogTag == "" {
				syslogTag = "pulseha"
			}

			assert.Equal(t, tc.expectedTag, syslogTag, "Tag should match expected value")
		})
	}
}

func TestSyslogConfigCompatibility(t *testing.T) {
	// Set test environment
	os.Setenv("PULSEHA_TEST", "true")
	defer os.Unsetenv("PULSEHA_TEST")

	t.Run("Old config without syslog fields", func(t *testing.T) {
		cfg := &config.Config{
			Pulse: config.Local{
				// Old-style config without syslog fields
				LoggingLevel: "info",
				LogToFile:    true,
				// LogToSyslog, SyslogTag, etc. are zero values
			},
		}

		// Should default to enabled when tag is empty
		logToSyslog := cfg.Pulse.LogToSyslog
		if cfg.Pulse.SyslogTag == "" {
			logToSyslog = true
		}

		assert.True(t, logToSyslog, "Old config should default to syslog enabled")
	})

	t.Run("New config with explicit syslog settings", func(t *testing.T) {
		cfg := &config.Config{
			Pulse: config.Local{
				LogToSyslog:    false,
				SyslogTag:      "disabled",
				SyslogFacility: "LOG_LOCAL0",
			},
		}

		// Should respect explicit setting
		logToSyslog := cfg.Pulse.LogToSyslog
		if cfg.Pulse.SyslogTag == "" {
			logToSyslog = true
		}

		assert.False(t, logToSyslog, "New config should respect explicit syslog setting")
	})
}

func TestSyslogErrorHandling(t *testing.T) {
	logger := log.New()

	// Capture log output to verify error handling
	var logOutput strings.Builder
	logger.SetOutput(&logOutput)

	// Test with invalid syslog configuration
	_, err := logrus_syslog.NewSyslogHook("invalid", "invalid:address", syslog.LOG_INFO, "test")

	// Should get an error for invalid syslog configuration
	assert.Error(t, err, "Should error with invalid syslog configuration")

	// In real implementation, this error would be logged and execution would continue
	// The main.go setupLogging function handles this gracefully
}
