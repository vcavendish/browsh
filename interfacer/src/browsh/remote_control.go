package browsh

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
)

var (
	remoteControlUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			host, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				host = r.Host
			}
			return host == "localhost" || host == "127.0.0.1" || host == "::1"
		},
		ReadBufferSize:  65536,
		WriteBufferSize: 65536,
	}
	remoteClients      = make(map[*websocket.Conn]bool)
	remoteClientsMutex sync.RWMutex
)

// RemoteCommand represents a command from external client
type RemoteCommand struct {
	Command string `json:"command"`
	Args    string `json:"args,omitempty"`
}

// RemoteResponse represents a response to external client
type RemoteResponse struct {
	Success bool        `json:"success"`
	Command string      `json:"command"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// StartRemoteControlServer starts the external command API server
func StartRemoteControlServer() {
	slog.Info("Checking remote-control flag", "value", viper.GetBool("remote-control"))
	if !viper.GetBool("remote-control") {
		return
	}

	serverMux := http.NewServeMux()
	serverMux.HandleFunc("/", remoteControlHandler)
	port := viper.GetInt("remote-control-port")
	slog.Info("Starting remote control server...", "port", port)
	fmt.Printf("\n*** Remote control server starting on port %d ***\n\n", port)

	go func() {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			slog.Error("Error starting remote control server", "error", err)
			fmt.Fprintf(os.Stderr, "ERROR starting remote control: %v\n", err)
			return
		}
		fmt.Fprintf(os.Stderr, "Remote control listening on :%d\n", port)
		if err := http.Serve(ln, serverMux); err != nil {
			slog.Error("Error serving remote control", "error", err)
		}
	}()
}

func remoteControlHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := remoteControlUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Remote control upgrade error", "error", err)
		return
	}

	remoteClientsMutex.Lock()
	remoteClients[ws] = true
	remoteClientsMutex.Unlock()

	slog.Info("Remote control client connected")

	defer func() {
		remoteClientsMutex.Lock()
		delete(remoteClients, ws)
		remoteClientsMutex.Unlock()
		ws.Close()
	}()

	for {
		_, message, err := ws.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				slog.Info("Remote control client disconnected")
			} else {
				slog.Error("Remote control read error", "error", err)
			}
			return
		}

		response := handleRemoteCommand(message)
		responseJSON, err := json.Marshal(response)
		if err != nil {
			errResp := `{"success":false,"error":"Failed to marshal response"}`
			ws.WriteMessage(websocket.TextMessage, []byte(errResp))
			continue
		}
		ws.WriteMessage(websocket.TextMessage, responseJSON)
	}
}

func handleRemoteCommand(message []byte) RemoteResponse {
	var cmd RemoteCommand
	if err := json.Unmarshal(message, &cmd); err != nil {
		return RemoteResponse{Success: false, Error: "Invalid JSON"}
	}

	slog.Info("Remote command received", "command", cmd.Command, "args", cmd.Args)

	switch cmd.Command {
	case "navigate", "goto":
		return handleNavigate(cmd.Args)
	case "click":
		return handleClick(cmd.Args)
	case "type":
		return handleType(cmd.Args)
	case "press":
		return handleKeyPress(cmd.Args)
	case "scroll_down":
		sendMessageToWebExtension("/tab_command,/scroll_down")
		return RemoteResponse{Success: true, Command: cmd.Command}
	case "scroll_up":
		sendMessageToWebExtension("/tab_command,/scroll_up")
		return RemoteResponse{Success: true, Command: cmd.Command}
	case "back":
		sendMessageToWebExtension("/tab_command,/history_back")
		return RemoteResponse{Success: true, Command: cmd.Command}
	case "forward":
		sendMessageToWebExtension("/tab_command,/history_forward")
		return RemoteResponse{Success: true, Command: cmd.Command}
	case "reload":
		sendMessageToWebExtension("/tab_command,/reload")
		return RemoteResponse{Success: true, Command: cmd.Command}
	case "screenshot":
		return handleScreenshot()
	case "get_state":
		return handleGetState()
	case "evaluate":
		return handleEvaluate(cmd.Args)
	case "query_selector":
		return handleQuerySelector(cmd.Args)
	case "get_snapshot":
		return handleGetSnapshot()
	case "get_aria_snapshot":
		return handleGetAriaSnapshot()
	case "click_ref":
		return handleClickRef(cmd.Args)
	case "shutdown":
		slog.Info("Shutdown command received, exiting")
		go func() {
			time.Sleep(100 * time.Millisecond)
			killFirefox()
			os.Exit(0)
		}()
		return RemoteResponse{Success: true, Command: cmd.Command}
	case "version":
		return RemoteResponse{Success: true, Command: cmd.Command, Data: browshVersion}
	default:
		return RemoteResponse{Success: false, Command: cmd.Command, Error: "Unknown command"}
	}
}

func handleNavigate(url string) RemoteResponse {
	if url == "" {
		return RemoteResponse{Success: false, Command: "navigate", Error: "URL required"}
	}
	sendMessageToWebExtension("/url_bar," + url)
	return RemoteResponse{Success: true, Command: "navigate", Data: url}
}

func handleClick(args string) RemoteResponse {
	// Args format: "x,y"
	parts := strings.Split(args, ",")
	if len(parts) != 2 {
		return RemoteResponse{Success: false, Command: "click", Error: "Format: x,y"}
	}

	// Create stdin event for click
	event := fmt.Sprintf(`{"button":1,"mouse_x":%s,"mouse_y":%s,"modifiers":0}`, parts[0], parts[1])
	sendMessageToWebExtension("/stdin," + event)
	return RemoteResponse{Success: true, Command: "click"}
}

func handleType(text string) RemoteResponse {
	// Send each character as a stdin event
	for _, char := range text {
		event := fmt.Sprintf(`{"char":"%c","modifiers":0}`, char)
		sendMessageToWebExtension("/stdin," + event)
	}
	return RemoteResponse{Success: true, Command: "type"}
}

func handleKeyPress(key string) RemoteResponse {
	event := fmt.Sprintf(`{"key":"%s","modifiers":0}`, key)
	sendMessageToWebExtension("/stdin," + event)
	return RemoteResponse{Success: true, Command: "press"}
}

func handleScreenshot() RemoteResponse {
	if err := switchToContentTab(); err != nil {
		return RemoteResponse{Success: false, Command: "screenshot", Error: "Failed to switch tab: " + err.Error()}
	}

	errMsg, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:TakeScreenshot",
		map[string]interface{}{},
		10*time.Second,
	)
	if err != nil {
		return RemoteResponse{Success: false, Command: "screenshot", Error: err.Error()}
	}
	if string(errMsg) != "null" {
		return RemoteResponse{Success: false, Command: "screenshot", Error: string(errMsg)}
	}

	var resultMap map[string]interface{}
	if err := json.Unmarshal(result, &resultMap); err != nil {
		return RemoteResponse{Success: false, Command: "screenshot", Error: "Failed to parse result"}
	}
	if val, ok := resultMap["value"]; ok {
		return RemoteResponse{Success: true, Command: "screenshot", Data: val}
	}
	return RemoteResponse{Success: false, Command: "screenshot", Error: "No screenshot data"}
}

func handleGetState() RemoteResponse {
	if CurrentTab == nil {
		return RemoteResponse{Success: false, Command: "get_state", Error: "No active tab"}
	}

	state := map[string]interface{}{
		"url":   CurrentTab.URI,
		"title": CurrentTab.Title,
	}
	return RemoteResponse{Success: true, Command: "get_state", Data: state}
}

// BroadcastToRemoteClients sends a message to all connected remote control clients
func BroadcastToRemoteClients(message interface{}) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}

	remoteClientsMutex.RLock()
	defer remoteClientsMutex.RUnlock()

	for client := range remoteClients {
		client.WriteMessage(websocket.TextMessage, data)
	}
}

// NotifyRemoteClients sends tab state updates to remote clients
func NotifyRemoteClients(tabState interface{}) {
	notification := map[string]interface{}{
		"event": "tab_state",
		"data":  tabState,
	}
	BroadcastToRemoteClients(notification)
}

// switchToContentTab switches Marionette to the last window handle (the content tab)
func switchToContentTab() error {
	// Get all window handles
	_, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:GetWindowHandles",
		map[string]interface{}{},
		5*time.Second,
	)
	if err != nil {
		return err
	}

	var handles []string
	if err := json.Unmarshal(result, &handles); err != nil {
		return fmt.Errorf("parse handles: %w", err)
	}
	if len(handles) == 0 {
		return fmt.Errorf("no window handles")
	}

	// Switch to the last handle (content tab — Browsh opens it after the initial about:blank)
	targetHandle := handles[len(handles)-1]
	slog.Info("Switching Marionette to content tab", "handle", targetHandle, "total", len(handles))
	_, _, err = sendFirefoxCommandWithResponse(
		"WebDriver:SwitchToWindow",
		map[string]interface{}{"handle": targetHandle},
		5*time.Second,
	)
	return err
}

// handleEvaluate executes JavaScript in the page via Marionette and returns result
func handleEvaluate(script string) RemoteResponse {
	if script == "" {
		return RemoteResponse{Success: false, Command: "evaluate", Error: "Script required"}
	}

	// Switch to the active content tab
	if err := switchToContentTab(); err != nil {
		return RemoteResponse{Success: false, Command: "evaluate", Error: "Failed to switch tab: " + err.Error()}
	}

	errMsg, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:ExecuteScript",
		map[string]interface{}{"script": "return " + script},
		10*time.Second,
	)
	if err != nil {
		return RemoteResponse{Success: false, Command: "evaluate", Error: err.Error()}
	}

	// Check for Marionette error
	if string(errMsg) != "null" {
		var mErr map[string]interface{}
		json.Unmarshal(errMsg, &mErr)
		errText := fmt.Sprintf("%v", mErr)
		return RemoteResponse{Success: false, Command: "evaluate", Error: errText}
	}

	// Parse result — Marionette returns {"value": <result>}
	var resultMap map[string]interface{}
	if err := json.Unmarshal(result, &resultMap); err != nil {
		return RemoteResponse{Success: true, Command: "evaluate", Data: string(result)}
	}
	if val, ok := resultMap["value"]; ok {
		return RemoteResponse{Success: true, Command: "evaluate", Data: val}
	}
	return RemoteResponse{Success: true, Command: "evaluate", Data: resultMap}
}

// handleQuerySelector finds element by CSS selector via Marionette
func handleQuerySelector(selector string) RemoteResponse {
	if selector == "" {
		return RemoteResponse{Success: false, Command: "query_selector", Error: "Selector required"}
	}

	if err := switchToContentTab(); err != nil {
		return RemoteResponse{Success: false, Command: "query_selector", Error: "Failed to switch tab: " + err.Error()}
	}

	script := fmt.Sprintf(`
		var el = document.querySelector(%q);
		if (!el) return null;
		var rect = el.getBoundingClientRect();
		return {
			tag: el.tagName.toLowerCase(),
			id: el.id || null,
			className: el.className || null,
			text: el.textContent.substring(0, 200),
			href: el.href || null,
			rect: { x: rect.x, y: rect.y, width: rect.width, height: rect.height }
		};
	`, selector)

	errMsg, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:ExecuteScript",
		map[string]interface{}{"script": script},
		10*time.Second,
	)
	if err != nil {
		return RemoteResponse{Success: false, Command: "query_selector", Error: err.Error()}
	}
	if string(errMsg) != "null" {
		return RemoteResponse{Success: false, Command: "query_selector", Error: string(errMsg)}
	}

	var resultMap map[string]interface{}
	if err := json.Unmarshal(result, &resultMap); err != nil {
		return RemoteResponse{Success: true, Command: "query_selector", Data: string(result)}
	}
	if val, ok := resultMap["value"]; ok {
		return RemoteResponse{Success: true, Command: "query_selector", Data: val}
	}
	return RemoteResponse{Success: true, Command: "query_selector", Data: resultMap}
}

// handleGetSnapshot returns an accessibility-like snapshot of the page via Marionette
func handleGetSnapshot() RemoteResponse {
	if err := switchToContentTab(); err != nil {
		return RemoteResponse{Success: false, Command: "get_snapshot", Error: "Failed to switch tab: " + err.Error()}
	}

	script := `
		function buildSnapshot(root) {
			var walker = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT);
			var items = [];
			var refCount = 0;
			var node;
			while (node = walker.nextNode()) {
				var role = node.getAttribute('role') || node.tagName.toLowerCase();
				var name = node.getAttribute('aria-label') || node.getAttribute('title') || 
				           node.getAttribute('alt') || '';
				if (!name && (role === 'a' || role === 'link')) name = node.textContent.trim().substring(0, 100);
				if (!name && (role === 'button' || role === 'input' || role === 'select' || role === 'textarea'))
					name = node.textContent.trim().substring(0, 100) || node.value || '';
				if (!name && role === 'img') name = node.src || '';
				
				var rect = node.getBoundingClientRect();
				if (rect.width === 0 && rect.height === 0) continue;
				
				var interactive = ['a','button','input','select','textarea','details','summary'].indexOf(node.tagName.toLowerCase()) >= 0;
				var hasRole = node.hasAttribute('role');
				var isVisible = rect.width > 0 && rect.height > 0 && getComputedStyle(node).visibility !== 'hidden';
				
				if (!isVisible) continue;
				if (!interactive && !hasRole && !name) continue;
				
				refCount++;
				items.push({
					ref: 'e' + refCount,
					role: role,
					name: name.substring(0, 200),
					rect: { x: Math.round(rect.x), y: Math.round(rect.y), w: Math.round(rect.width), h: Math.round(rect.height) }
				});
				if (refCount >= 100) break;
			}
			return items;
		}
		return { url: location.href, title: document.title, elements: buildSnapshot(document.body) };
	`

	errMsg, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:ExecuteScript",
		map[string]interface{}{"script": script},
		10*time.Second,
	)
	if err != nil {
		return RemoteResponse{Success: false, Command: "get_snapshot", Error: err.Error()}
	}
	if string(errMsg) != "null" {
		return RemoteResponse{Success: false, Command: "get_snapshot", Error: string(errMsg)}
	}

	var resultMap map[string]interface{}
	if err := json.Unmarshal(result, &resultMap); err != nil {
		return RemoteResponse{Success: true, Command: "get_snapshot", Data: string(result)}
	}
	if val, ok := resultMap["value"]; ok {
		return RemoteResponse{Success: true, Command: "get_snapshot", Data: val}
	}
	return RemoteResponse{Success: true, Command: "get_snapshot", Data: resultMap}
}

// lastAriaRefMap stores the ref→element mapping from the most recent ARIA snapshot
// Used by click_ref to resolve element references
var lastAriaRefMap map[string]interface{}
var lastAriaRefMapMutex sync.Mutex

// handleGetAriaSnapshot returns a Playwright-compatible ARIA snapshot via Marionette
func handleGetAriaSnapshot() RemoteResponse {
	if err := switchToContentTab(); err != nil {
		return RemoteResponse{Success: false, Command: "get_aria_snapshot", Error: "Failed to switch tab: " + err.Error()}
	}

	script := `return (() => {
  var refCounter = 0;
  var lines = [];
  function nextRef() { return 'e' + (++refCounter); }
  function getRole(el) {
    if (el.computedRole && el.computedRole !== 'generic' && el.computedRole !== 'none') return el.computedRole;
    var role = el.getAttribute && el.getAttribute('role');
    if (role) return role;
    return implicitRole(el);
  }
  function implicitRole(el) {
    var tag = el.tagName ? el.tagName.toLowerCase() : '';
    if (!tag) return 'generic';
    if (tag === 'a' && el.hasAttribute('href')) return 'link';
    if (tag === 'a') return 'generic';
    if (tag === 'button') return 'button';
    if (tag === 'input') return inputRole(el);
    if (tag === 'select') return 'combobox';
    if (tag === 'textarea') return 'textbox';
    if (tag === 'img') return 'img';
    if (tag === 'nav') return 'navigation';
    if (tag === 'main') return 'main';
    if (tag === 'header') return 'banner';
    if (tag === 'footer') return 'contentinfo';
    if (tag === 'aside') return 'complementary';
    if (tag === 'section') return el.getAttribute('aria-label') ? 'region' : 'generic';
    if (tag === 'form') return 'form';
    if (tag === 'ul' || tag === 'ol') return 'list';
    if (tag === 'li') return 'listitem';
    if (tag === 'table') return 'table';
    if (tag === 'tr') return 'row';
    if (tag === 'td') return 'cell';
    if (tag === 'th') return 'columnheader';
    if (/^h[1-6]$/.test(tag)) return 'heading';
    if (tag === 'p') return 'paragraph';
    if (tag === 'strong' || tag === 'b') return 'strong';
    if (tag === 'em' || tag === 'i') return 'emphasis';
    if (tag === 'code') return 'code';
    if (tag === 'iframe') return 'iframe';
    if (tag === 'dialog') return 'dialog';
    return 'generic';
  }
  function inputRole(el) {
    var type = (el.getAttribute('type') || 'text').toLowerCase();
    if (type === 'checkbox') return 'checkbox';
    if (type === 'radio') return 'radio';
    if (type === 'range') return 'slider';
    if (type === 'button' || type === 'submit' || type === 'reset') return 'button';
    if (type === 'search') return 'searchbox';
    return 'textbox';
  }
  function getName(el) {
    var ariaLabel = el.getAttribute && el.getAttribute('aria-label');
    if (ariaLabel) return ariaLabel;
    var labelledBy = el.getAttribute && el.getAttribute('aria-labelledby');
    if (labelledBy) {
      var parts = labelledBy.split(/\s+/).map(function(id) {
        var ref = el.ownerDocument.getElementById(id);
        return ref ? ref.textContent.trim() : '';
      }).filter(Boolean);
      if (parts.length) return parts.join(' ');
    }
    if (el.tagName && el.tagName.toLowerCase() === 'img') return el.getAttribute('alt') || '';
    if (el.tagName && (el.tagName.toLowerCase() === 'input' || el.tagName.toLowerCase() === 'textarea')) {
      var label = el.labels && el.labels[0];
      if (label) return label.textContent.trim();
      return el.getAttribute('placeholder') || '';
    }
    var title = el.getAttribute && el.getAttribute('title');
    if (title) return title;
    if (typeof el.computedLabel === 'string' && el.computedLabel) return el.computedLabel;
    var role = getRole(el);
    if (role === 'link' || role === 'button' || role === 'heading') {
      var text = el.textContent ? el.textContent.trim().replace(/\s+/g, ' ') : '';
      if (text && text.length < 200) return text;
    }
    return '';
  }
  function getDirectText(el) {
    var text = '';
    for (var i = 0; i < el.childNodes.length; i++) {
      var child = el.childNodes[i];
      if (child.nodeType === 3) {
        var t = child.textContent.replace(/\s+/g, ' ').trim();
        if (t) text += (text ? ' ' : '') + t;
      }
    }
    return text;
  }
  function isVisible(el) {
    if (!el.offsetParent && el.tagName && el.tagName.toLowerCase() !== 'body' && el.tagName.toLowerCase() !== 'html') return false;
    var style = window.getComputedStyle(el);
    if (style.display === 'none' || style.visibility === 'hidden' || style.opacity === '0') return false;
    return true;
  }
  function getAttributes(el, role) {
    var attrs = [];
    if (role === 'heading') {
      var tag = el.tagName ? el.tagName.toLowerCase() : '';
      var m = tag.match(/^h(\d)$/);
      if (m) attrs.push('[level=' + m[1] + ']');
    }
    var checked = el.getAttribute('aria-checked') || (el.checked !== undefined ? String(el.checked) : null);
    if (checked === 'true' || checked === 'false' || checked === 'mixed') attrs.push('[checked=' + checked + ']');
    if (el.disabled || el.getAttribute('aria-disabled') === 'true') attrs.push('[disabled]');
    var expanded = el.getAttribute('aria-expanded');
    if (expanded === 'true' || expanded === 'false') attrs.push('[expanded=' + expanded + ']');
    var selected = el.getAttribute('aria-selected');
    if (selected === 'true' || selected === 'false') attrs.push('[selected=' + selected + ']');
    var pressed = el.getAttribute('aria-pressed');
    if (pressed === 'true' || pressed === 'false' || pressed === 'mixed') attrs.push('[pressed=' + pressed + ']');
    if (role === 'link' || role === 'button') {
      var cursor = window.getComputedStyle(el).cursor;
      if (cursor === 'pointer') attrs.push('[cursor=pointer]');
    }
    return attrs;
  }
  function buildTree(el, depth, refMap) {
    if (!el || el.nodeType !== 1) return;
    var tag = el.tagName ? el.tagName.toLowerCase() : '';
    if (['script','style','noscript','template','meta','link','br','hr'].indexOf(tag) >= 0) return;
    if (!isVisible(el)) return;
    var role = getRole(el);
    var name = getName(el);
    var ref = nextRef();
    var attrs = getAttributes(el, role);
    var rect = el.getBoundingClientRect();
    refMap[ref] = { role: role, name: name, x: rect.x, y: rect.y, w: rect.width, h: rect.height };
    var isGeneric = role === 'generic' || role === 'none' || role === 'presentation';
    var hasName = name.length > 0;
    var hasAttrs = attrs.length > 0;
    var isInteresting = !isGeneric || hasName || hasAttrs;
    var childElements = [];
    for (var i = 0; i < el.children.length; i++) {
      var c = el.children[i];
      var ct = c.tagName ? c.tagName.toLowerCase() : '';
      if (['script','style','noscript','template','meta','link'].indexOf(ct) < 0 && isVisible(c)) childElements.push(c);
    }
    var directText = getDirectText(el);
    var hasChildren = childElements.length > 0;
    var indent = '';
    for (var d = 0; d < depth; d++) indent += '  ';
    if (!isInteresting) {
      if (directText && !hasChildren) { lines.push(indent + '- text: ' + directText); }
      else {
        if (directText) lines.push(indent + '- text: ' + directText);
        for (var ci = 0; ci < childElements.length; ci++) buildTree(childElements[ci], depth, refMap);
      }
      return;
    }
    var line = indent + '- ' + role;
    if (hasName) line += ' "' + name.replace(/"/g, '\\"') + '"';
    var nonCursorAttrs = attrs.filter(function(a) { return a !== '[cursor=pointer]'; });
    var cursorAttr = attrs.filter(function(a) { return a === '[cursor=pointer]'; })[0];
    if (nonCursorAttrs.length) line += ' ' + nonCursorAttrs.join(' ');
    line += ' [ref=' + ref + ']';
    if (cursorAttr) line += ' ' + cursorAttr;
    var hasUrl = (role === 'link' && el.href);
    var needsBlock = hasChildren || hasUrl;
    var nameMatchesText = hasName && name === directText;
    if (!needsBlock && directText && !nameMatchesText) {
      lines.push(line + ': ' + directText);
    } else if (needsBlock) {
      lines.push(line + ':');
      if (hasUrl) lines.push(indent + '  - /url: ' + el.href);
      for (var ni = 0; ni < el.childNodes.length; ni++) {
        var child = el.childNodes[ni];
        if (child.nodeType === 3) {
          var t = child.textContent.replace(/\s+/g, ' ').trim();
          if (t && t !== name) lines.push(indent + '  - text: ' + t);
        } else if (child.nodeType === 1) {
          var ct2 = child.tagName ? child.tagName.toLowerCase() : '';
          if (['script','style','noscript','template','meta','link'].indexOf(ct2) < 0 && isVisible(child))
            buildTree(child, depth + 1, refMap);
        }
      }
    } else {
      lines.push(line);
    }
  }
  var refMap = {};
  buildTree(document.body, 0, refMap);
  return { full: lines.join('\n'), refMap: refMap };
})()`

	errMsg, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:ExecuteScript",
		map[string]interface{}{"script": script},
		15*time.Second,
	)
	if err != nil {
		return RemoteResponse{Success: false, Command: "get_aria_snapshot", Error: err.Error()}
	}
	if string(errMsg) != "null" {
		return RemoteResponse{Success: false, Command: "get_aria_snapshot", Error: string(errMsg)}
	}

	var resultMap map[string]interface{}
	if err := json.Unmarshal(result, &resultMap); err != nil {
		return RemoteResponse{Success: true, Command: "get_aria_snapshot", Data: string(result)}
	}
	if val, ok := resultMap["value"]; ok {
		// Store refMap for click_ref resolution
		if valMap, ok := val.(map[string]interface{}); ok {
			if rm, ok := valMap["refMap"]; ok {
				lastAriaRefMapMutex.Lock()
				if rmMap, ok := rm.(map[string]interface{}); ok {
					lastAriaRefMap = rmMap
				}
				lastAriaRefMapMutex.Unlock()
			}
		}
		return RemoteResponse{Success: true, Command: "get_aria_snapshot", Data: val}
	}
	return RemoteResponse{Success: true, Command: "get_aria_snapshot", Data: resultMap}
}

// handleClickRef clicks an element by its ARIA snapshot ref (e.g., "e10")
func handleClickRef(ref string) RemoteResponse {
	if ref == "" {
		return RemoteResponse{Success: false, Command: "click_ref", Error: "Ref required (e.g., e10)"}
	}

	if err := switchToContentTab(); err != nil {
		return RemoteResponse{Success: false, Command: "click_ref", Error: "Failed to switch tab: " + err.Error()}
	}

	// Use evaluate to find and click the element by walking the DOM the same way the snapshot does
	script := fmt.Sprintf(`return (() => {
  var targetRef = '%s';
  var refCounter = 0;
  function getRole(el) {
    if (el.computedRole && el.computedRole !== 'generic' && el.computedRole !== 'none') return el.computedRole;
    var role = el.getAttribute && el.getAttribute('role');
    if (role) return role;
    var tag = el.tagName ? el.tagName.toLowerCase() : '';
    if (tag === 'a' && el.hasAttribute('href')) return 'link';
    if (tag === 'button') return 'button';
    if (/^h[1-6]$/.test(tag)) return 'heading';
    return 'generic';
  }
  function isVisible(el) {
    if (!el.offsetParent && el.tagName && el.tagName.toLowerCase() !== 'body' && el.tagName.toLowerCase() !== 'html') return false;
    var style = window.getComputedStyle(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && style.opacity !== '0';
  }
  function findByRef(el) {
    if (!el || el.nodeType !== 1) return null;
    var tag = el.tagName ? el.tagName.toLowerCase() : '';
    if (['script','style','noscript','template','meta','link','br','hr'].indexOf(tag) >= 0) return null;
    if (!isVisible(el)) return null;
    refCounter++;
    var ref = 'e' + refCounter;
    if (ref === targetRef) return el;
    for (var i = 0; i < el.children.length; i++) {
      var found = findByRef(el.children[i]);
      if (found) return found;
    }
    return null;
  }
  var el = findByRef(document.body);
  if (!el) return { error: 'Ref ' + targetRef + ' not found' };
  el.click();
  return { clicked: true, tag: el.tagName.toLowerCase(), text: (el.textContent || '').trim().substring(0, 100) };
})()`, ref)

	errMsg, result, err := sendFirefoxCommandWithResponse(
		"WebDriver:ExecuteScript",
		map[string]interface{}{"script": script},
		10*time.Second,
	)
	if err != nil {
		return RemoteResponse{Success: false, Command: "click_ref", Error: err.Error()}
	}
	if string(errMsg) != "null" {
		return RemoteResponse{Success: false, Command: "click_ref", Error: string(errMsg)}
	}

	var resultMap map[string]interface{}
	if err := json.Unmarshal(result, &resultMap); err != nil {
		return RemoteResponse{Success: true, Command: "click_ref", Data: string(result)}
	}
	if val, ok := resultMap["value"]; ok {
		if valMap, ok := val.(map[string]interface{}); ok {
			if errStr, ok := valMap["error"]; ok {
				return RemoteResponse{Success: false, Command: "click_ref", Error: fmt.Sprintf("%v", errStr)}
			}
		}
		return RemoteResponse{Success: true, Command: "click_ref", Data: val}
	}
	return RemoteResponse{Success: true, Command: "click_ref", Data: resultMap}
}
