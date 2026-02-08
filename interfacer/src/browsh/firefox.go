package browsh

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell"
	"github.com/go-errors/errors"
	"github.com/spf13/viper"
)

//go:embed browsh.xpi
var browshXpi embed.FS

var (
	marionette     net.Conn
	ffCommandCount = 0
	// Pending Marionette responses keyed by command ID
	marionetteResponses     = make(map[int]chan []byte)
	marionetteResponseMutex sync.Mutex
	defaultFFPrefs          = map[string]string{
		"startup.homepage_welcome_url.additional": "''",
		"devtools.errorconsole.enabled":           "true",
		"devtools.chrome.enabled":                 "true",

		// Send Browser Console (different from Devtools console) output to
		// STDOUT.
		"browser.dom.window.dump.enabled": "true",

		// From:
		// http://hg.mozilla.org/mozilla-central/file/1dd81c324ac7/build/automation.py.in//l388
		// Make url-classifier updates so rare that they won"t affect tests.
		"urlclassifier.updateinterval": "172800",
		// Point the url-classifier to a nonexistent local URL for fast failures.
		"browser.safebrowsing.provider.0.gethashURL": "'http://localhost/safebrowsing-dummy/gethash'",
		"browser.safebrowsing.provider.0.keyURL":     "'http://localhost/safebrowsing-dummy/newkey'",
		"browser.safebrowsing.provider.0.updateURL":  "'http://localhost/safebrowsing-dummy/update'",

		// Disable self repair/SHIELD
		"browser.selfsupport.url": "'https://localhost/selfrepair'",
		// Disable Reader Mode UI tour
		"browser.reader.detectedFirstArticle": "true",

		// Set the policy firstURL to an empty string to prevent
		// the privacy info page to be opened on every "web-ext run".
		// (See #1114 for rationale)
		"datareporting.policy.firstRunURL": "''",
	}
)

func startHeadlessFirefox() {
	slog.Info("Starting Firefox in headless mode")
	checkIfFirefoxIsAlreadyRunning()
	firefoxPath := ensureFirefoxBinary()
	ensureFirefoxVersion(firefoxPath)
	args := []string{"--marionette", "-remote-allow-system-access"}
	if !viper.GetBool("firefox.with-gui") {
		args = append(args, "--headless")
	}
	profile := viper.GetString("firefox.profile")
	if profile != "browsh-default" {
		slog.Info("Using Firefox profile", "profile", profile)
		args = append(args, "-P", profile)
	} else {
		profilePath := getFirefoxProfilePath()
		slog.Info("Using default profile", "path", profilePath)
		args = append(args, "--profile", profilePath)
	}
	firefoxCmd = exec.Command(firefoxPath, args...)
	stdout, err := firefoxCmd.StdoutPipe()
	if err != nil {
		Shutdown(err)
	}
	if err := firefoxCmd.Start(); err != nil {
		Shutdown(err)
	}

	// Handle signals so Firefox is killed when browsh receives SIGTERM/SIGINT.
	// Playwright sends these via taskkill /T (Windows) or process.kill (Unix).
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigChan
		slog.Info("Received signal, shutting down", "signal", sig)
		killFirefox()
		os.Exit(0)
	}()

	in := bufio.NewScanner(stdout)
	for in.Scan() {
		slog.Info("FF-CONSOLE", "stdout", in.Text())
	}
	// If Firefox exits (stdout closes), clean up the reference
	firefoxCmd = nil
}

func checkIfFirefoxIsAlreadyRunning() {
	if runtime.GOOS == "windows" {
		return
	}
	processes := Shell("ps aux")
	r, _ := regexp.Compile("firefox.*--headless")
	if r.MatchString(processes) {
		Shutdown(errors.New("A headless Firefox is already running"))
	}
}

func ensureFirefoxBinary() string {
	path := viper.GetString("firefox.path")
	if path == "firefox" {
		switch runtime.GOOS {
		case "windows":
			path = getFirefoxPath()
		case "darwin":
			path = "/Applications/Firefox.app/Contents/MacOS/firefox"
		default:
			path = getFirefoxPath()
		}
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = errors.New("Firefox binary not found: " + path)
		}
		Shutdown(err)
	}
	slog.Info("Using Firefox", "path", path)
	return path
}

// Taken from https://stackoverflow.com/a/18411978/575773
func versionOrdinal(version string) string {
	// ISO/IEC 14651:2011
	const maxByte = 1<<8 - 1
	vo := make([]byte, 0, len(version)+8)
	j := -1
	for i := 0; i < len(version); i++ {
		b := version[i]
		if '0' > b || b > '9' {
			vo = append(vo, b)
			j = -1
			continue
		}
		if j == -1 {
			vo = append(vo, 0x00)
			j = len(vo) - 1
		}
		if vo[j] == 1 && vo[j+1] == '0' {
			vo[j+1] = b
			continue
		}
		if vo[j]+1 > maxByte {
			panic("VersionOrdinal: invalid version")
		}
		vo = append(vo, b)
		vo[j]++
	}
	return string(vo)
}

// Start Firefox via the `web-ext` CLI tool. This is for development and testing,
// because I haven't been able to recreate the way `web-ext` injects an unsigned
// extension.
func startWERFirefox() {
	slog.Info("Attempting to start headless Firefox with `web-ext`")
	if IsConnectedToWebExtension {
		Shutdown(errors.New("There appears to already be an existing Web Extension connection"))
	}
	checkIfFirefoxIsAlreadyRunning()
	rootDir := Shell("git rev-parse --show-toplevel")
	args := []string{
		"run",
		"--firefox=" + rootDir + "/webext/contrib/firefoxheadless.sh",
		"--verbose",
		"--no-reload",
	}
	firefoxProcess := exec.Command(rootDir+"/webext/node_modules/.bin/web-ext", args...)
	firefoxProcess.Dir = rootDir + "/webext/dist/"
	stdout, err := firefoxProcess.StdoutPipe()
	if err != nil {
		Shutdown(err)
	}
	if err := firefoxProcess.Start(); err != nil {
		Shutdown(err)
	}
	in := bufio.NewScanner(stdout)
	for in.Scan() {
		if strings.Contains(in.Text(), "Connected to the remote Firefox debugger") {
		}
		if strings.Contains(in.Text(), "JavaScript strict") ||
			strings.Contains(in.Text(), "D-BUS") ||
			strings.Contains(in.Text(), "dbus") {
			continue
		}
		slog.Info("FF-CONSOLE", "stdout", in.Text())
	}
	slog.Info("WER Firefox unexpectedly closed")
}

// Connect to Firefox's Marionette service.
// RANT: Firefox's remote control tools are so confusing. There seem to be 2
// services that come with your Firefox binary; Marionette and the Remote
// Debugger. The latter you would expect to follow the widely supported
// Chrome standard, but no, it's merely on the roadmap. There is very little
// documentation on either. I have the impression, but I'm not sure why, that
// the Remote Debugger is better, seemingly more API methods, and as mentioned
// is on the roadmap to follow the Chrome standard.
// I've used Marionette here, simply because it was easier to reverse engineer
// from the Python Marionette package.
func firefoxMarionette() {
	var (
		err  error
		conn net.Conn
	)
	connected := false
	slog.Info("Attempting to connect to Firefox Marionette")
	start := time.Now()
	for time.Since(start) < 30*time.Second {
		conn, err = net.Dial("tcp", "127.0.0.1:2828")
		if err != nil {
			if !strings.Contains(err.Error(), "refused") {
				Shutdown(err)
			} else {
				time.Sleep(10 * time.Millisecond)
				continue
			}
		} else {
			connected = true
			break
		}
	}
	if !connected {
		Shutdown(errors.New("Failed to connect to Firefox's Marionette within 30 seconds"))
	}
	marionette = conn
	go listenMarionette()
	sendFirefoxCommand("WebDriver:NewSession", map[string]interface{}{})
}

func installWebextension() {
	data, err := browshXpi.ReadFile("browsh.xpi")
	if err != nil {
		Shutdown(err)
	}
	path := filepath.Join(os.TempDir(), "browsh-webext-addon")
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		Shutdown(err)
	}
	args := map[string]interface{}{"path": path}
	sendFirefoxCommand("Addon:Install", args)
}

func createUserJS() {
	profilePath := getFirefoxProfilePath()
	path := filepath.Join(profilePath, "user.js")
	slog.Info("Writing user.js", "path", path)
	f, err := os.Create(path)
	if err != nil {
		slog.Error("Could not create user.js", "error", err)
		return
	}
	defer f.Close()

	for key, value := range defaultFFPrefs {
		_, err := f.WriteString(fmt.Sprintf("user_pref(\"%s\", %s);\n", key, value))
		if err != nil {
			slog.Error("Error writing to user.js", "error", err)
		}
	}
	for _, pref := range viper.GetStringSlice("firefox.preferences") {
		parts := strings.SplitN(pref, "=", 2)
		if len(parts) == 2 {
			_, err := f.WriteString(fmt.Sprintf("user_pref(\"%s\", %s);\n", parts[0], parts[1]))
			if err != nil {
				slog.Error("Error writing to user.js", "error", err)
			}
		}
	}
}

// Set a Firefox preference as you would in `about:config`
// `value` needs to be supplied with quotes if it's to be used as a JS string
func setFFPreference(key string, value string) {
	var args map[string]interface{}
	var script string
	sendFirefoxCommand("Marionette:SetContext", map[string]interface{}{"value": "chrome"})
	script = fmt.Sprintf(`
		const { Preferences } = ChromeUtils.importESModule("resource://gre/modules/Preferences.sys.mjs");
		const prefs = new Preferences({defaultBranch: "root"});
		prefs.set("%s", %s);`, key, value)
	args = map[string]interface{}{"script": script}
	sendFirefoxCommand("WebDriver:ExecuteScript", args)
	sendFirefoxCommand("Marionette:SetContext", map[string]interface{}{"value": "content"})
}

// listenMarionette continuously reads Marionette responses and routes them.
// Response format: length:json_array where json_array is [1, id, error, result]
func listenMarionette() {
	reader := bufio.NewReader(marionette)
	for {
		// Read length prefix (digits followed by colon)
		lengthStr, err := reader.ReadString(':')
		if err != nil {
			slog.Error("Error reading Marionette length", "error", err)
			return
		}
		lengthStr = strings.TrimSuffix(lengthStr, ":")
		length, err := strconv.Atoi(lengthStr)
		if err != nil {
			slog.Error("Invalid Marionette length", "value", lengthStr)
			continue
		}

		// Read exactly `length` bytes of JSON
		data := make([]byte, length)
		n := 0
		for n < length {
			count, err := reader.Read(data[n:])
			if err != nil {
				slog.Error("Error reading Marionette data", "error", err)
				return
			}
			n += count
		}

		slog.Info("FF-MRNT", "response", string(data))

		// Parse response: [type, id, error, result]
		var response []json.RawMessage
		if err := json.Unmarshal(data, &response); err != nil {
			slog.Error("Failed to parse Marionette response", "error", err)
			continue
		}
		if len(response) < 4 {
			continue
		}

		// Extract command ID
		var cmdID int
		if err := json.Unmarshal(response[1], &cmdID); err != nil {
			continue
		}

		// Route to waiting caller if any
		marionetteResponseMutex.Lock()
		ch, exists := marionetteResponses[cmdID]
		if exists {
			delete(marionetteResponses, cmdID)
		}
		marionetteResponseMutex.Unlock()

		if exists {
			ch <- data
		}
	}
}

func sendFirefoxCommand(command string, args map[string]interface{}) {
	slog.Info("Sending command to Firefox Marionette", "command", command, "args", args)
	fullCommand := []interface{}{0, ffCommandCount, command, args}
	marshalled, _ := json.Marshal(fullCommand)
	message := fmt.Sprintf("%d:%s", len(marshalled), marshalled)
	fmt.Fprintf(marionette, "%s", message)
	ffCommandCount++
}

// sendFirefoxCommandWithResponse sends a Marionette command and waits for response
func sendFirefoxCommandWithResponse(command string, args map[string]interface{}, timeout time.Duration) (json.RawMessage, json.RawMessage, error) {
	ch := make(chan []byte, 1)
	cmdID := ffCommandCount

	marionetteResponseMutex.Lock()
	marionetteResponses[cmdID] = ch
	marionetteResponseMutex.Unlock()

	sendFirefoxCommand(command, args)

	select {
	case data := <-ch:
		var response []json.RawMessage
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, nil, fmt.Errorf("parse error: %w", err)
		}
		if len(response) < 4 {
			return nil, nil, fmt.Errorf("invalid response length")
		}
		return response[2], response[3], nil // error, result
	case <-time.After(timeout):
		marionetteResponseMutex.Lock()
		delete(marionetteResponses, cmdID)
		marionetteResponseMutex.Unlock()
		return nil, nil, fmt.Errorf("timeout after %v", timeout)
	}
}

func setDefaultFirefoxPreferences() {
	for key, value := range defaultFFPrefs {
		setFFPreference(key, value)
	}
	for _, pref := range viper.GetStringSlice("firefox.preferences") {
		parts := strings.SplitN(pref, "=", 2)
		setFFPreference(parts[0], parts[1])
	}
}

func beginTimeLimit() {
	warningLength := 10
	warningLimit := time.Duration(*timeLimit - warningLength)
	time.Sleep(warningLimit * time.Second)
	message := fmt.Sprintf("Browsh will close in %d seconds...", warningLength)
	sendMessageToWebExtension("/status," + message)
	time.Sleep(time.Duration(warningLength) * time.Second)
	quitBrowsh()
}

// Careful what you change here as it isn't tested during CI
func setupFirefox() {
	createUserJS()
	go startHeadlessFirefox()
	if *timeLimit > 0 {
		go beginTimeLimit()
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		quitBrowsh()
	}()

	firefoxMarionette()
	installWebextension()
}

func StartFirefox() {
	if !viper.GetBool("firefox.use-existing") {
		writeString(0, 16, "Waiting for Firefox to connect...", tcell.StyleDefault)
		if IsTesting {
			writeString(0, 17, "TEST MODE", tcell.StyleDefault)
			go startWERFirefox()
			firefoxMarionette()
		} else {
			setupFirefox()
		}
	} else {
		firefoxMarionette()
		writeString(0, 16, "Waiting for a user-initiated Firefox instance to connect...", tcell.StyleDefault)
	}
}

func quitFirefox() {
	sendFirefoxCommand("Marionette:Quit", map[string]interface{}{})
}
