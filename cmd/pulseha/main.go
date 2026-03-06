// PulseHA - HA Cluster Daemon
// Copyright (C) 2017-2021  Andrew Zak <andrew@linux.com>
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published
// by the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"io"
	"log/syslog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/syleron/pulseha/internal/membership"
	"github.com/syleron/pulseha/internal/server"
	"github.com/syleron/pulseha/packages/config"
)

var (
	Version string
	Build   string
)

func main() {

	// Draw logo
	buildStr := "unknown"
	if len(Build) >= 7 {
		buildStr = Build[0:7]
	}
	fmt.Printf(`
   ___       _                  _
  / _ \_   _| |___  ___  /\  /\/_\
 / /_)/ | | | / __|/ _ \/ /_/ //_\\
/ ___/| |_| | \__ \  __/ __  /  _  \  Version %s
\/     \__,_|_|___/\___\/ /_/\_/ \_/  Build   %s

`, Version, buildStr)

	// Initialize logger with date/time and PID prefix.
	logger := log.NewWithOptions(os.Stdout, log.Options{
		ReportTimestamp: true,
		TimeFormat:      time.DateTime,
		Prefix:          "(" + strconv.Itoa(os.Getpid()) + ")",
	})
	logger.SetFormatter(log.TextFormatter)

	// Set default logging level to debug during development
	logger.SetLevel(log.DebugLevel)

	// Load config
	cfg, err := config.New()
	if err != nil {
		logger.Fatal("Failed to load config", "error", err)
	}

	// Override logging level from config if specified
	if cfg.Pulse.LoggingLevel != "" {
		if level, err := log.ParseLevel(cfg.Pulse.LoggingLevel); err == nil {
			logger.SetLevel(level)
		} else {
			logger.Warn("Invalid logging level in config, using debug", "level", cfg.Pulse.LoggingLevel)
		}
	}

	// Setup logging (syslog by default + file if enabled)
	if err := setupLogging(cfg, logger); err != nil {
		logger.Fatal(err)
	}

	// Initialize member list
	memberList := membership.NewMemberList(cfg, logger)

	// Initialize health checker
	healthChecker := membership.NewHealthChecker(memberList, logger)

	// Start pprof server for memory profiling (only in debug mode)
	if os.Getenv("PULSEHA_DEBUG") == "true" {
		go func() {
			logger.Debug("Starting pprof server on :6060 for memory profiling")
			if err := http.ListenAndServe(":6060", nil); err != nil {
				logger.Error("pprof server failed", "error", err)
			}
		}()
	}

	// Create and start server
	srv := server.NewServer(cfg, logger, memberList, healthChecker)
	if err := srv.Start(); err != nil {
		logger.Fatal("Failed to start server", "error", err)
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR2)

	// Create a WaitGroup to ensure graceful shutdown
	var wg sync.WaitGroup

	// Add error channel to catch shutdown errors
	errChan := make(chan error, 1)

	for {
		sig := <-sigChan
		switch sig {
		case syscall.SIGUSR2:
			// Reload configuration
			logger.Info("Reloading configuration...")
			if err := cfg.Reload(); err != nil {
				logger.Error("Failed to reload config", "error", err)
				continue
			}
			// Restart server with new config
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.Stop()
				srv = server.NewServer(cfg, logger, memberList, healthChecker)
				if err := srv.Start(); err != nil {
					errChan <- fmt.Errorf("failed to restart server: %v", err)
				}
			}()

		case syscall.SIGINT, syscall.SIGTERM:
			logger.Info("Initiating graceful shutdown...")

			// Stop health checker first
			logger.Debug("Stopping health checker...")
			healthChecker.Stop()

			// Stop server
			logger.Debug("Stopping server...")
			wg.Add(1)
			go func() {
				defer wg.Done()
				srv.Stop()
			}()

			// Wait for all components to shut down
			logger.Debug("Waiting for all components to shut down...")
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			// Wait for shutdown with timeout
			select {
			case <-done:
				logger.Info("Graceful shutdown completed")
				os.Exit(0)
			case err := <-errChan:
				logger.Error("Error during shutdown", "error", err)
				os.Exit(1)
			case <-time.After(10 * time.Second):
				logger.Error("Shutdown timed out, forcing exit")
				os.Exit(1)
			}
		}
	}
}

func setupLogging(cfg *config.Config, logger *log.Logger) error {
	var writers []io.Writer

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

		var sysw *syslog.Writer
		var err error
		if cfg.Pulse.SyslogNetwork != "" || cfg.Pulse.SyslogAddress != "" {
			sysw, err = syslog.Dial(cfg.Pulse.SyslogNetwork, cfg.Pulse.SyslogAddress, facility, syslogTag)
		} else {
			sysw, err = syslog.New(facility, syslogTag)
		}
		if err != nil {
			// If syslog is not available (e.g., in containers), log warning but continue
			logger.Warn("Failed to connect to syslog, continuing without syslog", "error", err)
		} else {
			logger.Info("Syslog logging enabled")
			writers = append(writers, sysw)
		}
	}

	// Setup file logging if enabled
	if cfg.Pulse.LogToFile {
		// Restrict permissions so that only the owner can modify the log and
		// the group can read it. This avoids exposing potentially sensitive
		// log output to the world.
		logFile, err := os.OpenFile(cfg.Pulse.LogFileLocation, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0640)
		if err != nil {
			return fmt.Errorf("failed to open log file: %v", err)
		}
		logger.Info("File logging enabled to path: " + cfg.Pulse.LogFileLocation)
		writers = append(writers, logFile)
	}

	// Always add stdout to the list of logging destinations.
	writers = append(writers, os.Stdout)

	// Set multi-writer output (stdout + file if enabled)
	if len(writers) > 1 {
		mw := io.MultiWriter(writers...)
		logger.SetOutput(mw)
	} else {
		logger.SetOutput(writers[0])
	}

	return nil
}
