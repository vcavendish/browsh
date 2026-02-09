package browsh

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/shibukawa/configdir"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var (
	configFilename = "config.toml"

	isDebug   = pflag.Bool("debug", false, "slog.Info to ./debug.log")
	timeLimit = pflag.Int("time-limit", 0, "Kill Browsh after the specified number of seconds")
	_         = pflag.Bool("http-server-mode", false, "Run as an HTTP service")

	_ = pflag.String("startup-url", "https://www.brow.sh", "URL to launch at startup")
	_ = pflag.String("firefox.path", "firefox", "Path to Firefox executable")
	_ = pflag.Bool("firefox.with-gui", false, "Don't use headless Firefox")
	_ = pflag.Bool("firefox.use-existing", false, "Whether Browsh should launch Firefox or not")
	_ = pflag.Bool("monochrome", false, "Start browsh in monochrome mode")
	_ = pflag.Bool("name", false, "Print out the name: Browsh")
	_ = pflag.Bool("render-only", false, "Only start WebSocket server and terminal renderer, skip Firefox launch")
	_ = pflag.Bool("remote-control", false, "Enable external command API on port 3335")
	_ = pflag.Int("remote-control-port", 3335, "Port for external command API")
	_ = pflag.Int("marionette-port", 2828, "Port for Firefox Marionette protocol (0 = auto-assign)")
	_ = pflag.Int("websocket-port", 0, "Port for webextension WebSocket (0 = use config default)")

	// marionettePort is the resolved port used for Firefox Marionette connections
	marionettePort = 2828
	// tempProfilePath holds the temporary Firefox profile directory (cleaned up on shutdown)
	tempProfilePath string
)

func getConfigNamespace() string {
	if IsTesting {
		return "browsh-testing"
	}
	return "browsh"
}

// Gets a cross-platform path to a folder containing Browsh config
func getConfigDir() string {
	marker := "browsh-settings"
	// configdir has no other option but to have a nested folder
	configDirs := configdir.New(getConfigNamespace(), marker)
	folders := configDirs.QueryFolders(configdir.Global)
	// Delete the previously enforced nested folder
	path := strings.Trim(folders[0].Path, marker)
	os.MkdirAll(path, os.ModePerm)
	ensureConfigFile(path)
	return path
}

// Copy the sample config file if the user doesn't already have a config file
func ensureConfigFile(path string) {
	fullPath := filepath.Join(path, configFilename)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		file, err := os.Create(fullPath)
		if err != nil {
			Shutdown(err)
		}
		defer file.Close()
		_, err = file.WriteString(configSample)
		if err != nil {
			Shutdown(err)
		}
	}
}

// Gets a cross-platform path to store a Browsh-specific Firefox profile
func getFirefoxProfilePath() string {
	configDirs := configdir.New(getConfigNamespace(), "firefox_profile")
	folders := configDirs.QueryFolders(configdir.Global)
	folders[0].MkdirAll()
	return folders[0].Path
}

func setDefaults() {
	// Temporary experimental configurable keybindings
	viper.SetDefault("tty.keys.next-tab", []string{"\u001c", "28", "2"})
}

func loadConfig() {
	dir := getConfigDir()
	fullPath := filepath.Join(dir, configFilename)
	slog.Info("Looking in " + fullPath + " for config.")
	viper.SetConfigType("toml")
	viper.SetConfigName(strings.Trim(configFilename, ".toml"))
	viper.AddConfigPath(dir)
	viper.AddConfigPath(".")
	setDefaults()
	// First load the sample config in case the user hasn't updated any new fields
	if err := viper.ReadConfig(bytes.NewBuffer([]byte(configSample))); err != nil {
		panic(fmt.Errorf("Config file error: %s \n", err))
	}
	// Then load the users own config file, overwriting the sample config
	if err := viper.MergeInConfig(); err != nil {
		panic(fmt.Errorf("Config file error: %s \n", err))
	}
	viper.BindPFlags(pflag.CommandLine)
}

// findFreePort asks the OS for a free TCP port by binding to :0.
func findFreePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		Shutdown(fmt.Errorf("findFreePort: %w", err))
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// resolveDynamicPorts resolves marionette and websocket ports.
// A value of 0 means auto-assign a free port.
func resolveDynamicPorts() {
	mp := viper.GetInt("marionette-port")
	if mp == 0 {
		marionettePort = findFreePort()
		slog.Info("Auto-assigned marionette port", "port", marionettePort)
	} else {
		marionettePort = mp
	}

	wp := viper.GetInt("websocket-port")
	if wp != 0 {
		// CLI flag overrides config with explicit port
		viper.Set("browsh.websocket-port", fmt.Sprintf("%d", wp))
		slog.Info("Websocket port set via flag", "port", wp)
	} else if pflag.CommandLine.Changed("websocket-port") {
		// Explicit --websocket-port=0 means auto-assign
		viper.Set("browsh.websocket-port", "0")
	}
	// Check if the resolved websocket port is "0" (string, from config or flag)
	wsPort := viper.GetString("browsh.websocket-port")
	if wsPort == "0" {
		freePort := findFreePort()
		viper.Set("browsh.websocket-port", fmt.Sprintf("%d", freePort))
		slog.Info("Auto-assigned websocket port", "port", freePort)
	}
}

// cleanupTempProfile removes the temporary Firefox profile directory if one was created.
func cleanupTempProfile() {
	if tempProfilePath != "" {
		slog.Info("Cleaning up temp Firefox profile", "path", tempProfilePath)
		os.RemoveAll(tempProfilePath)
		tempProfilePath = ""
	}
}
