package gateway

import (
	"os"
	"strings"
	"testing"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("assets/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The composer follows the send capability; tmag's gate decides the state.
func TestComposerFollowsSendCapability(t *testing.T) {
	page := readAsset(t, "app.js")
	if strings.Contains(page, `caps.includes("send") && x.s.state === "idle"`) {
		t.Fatal("the composer is limited to idle sessions")
	}
	if !strings.Contains(page, `const canSend = caps.includes("send");`) {
		t.Fatal("the composer does not follow the send capability")
	}
}

// A tap carries the alert's route to an open page without WebKit's navigate,
// or opens one at a route the page accepts.
func TestNotificationTapCarriesTheRoute(t *testing.T) {
	sw := readAsset(t, "sw.js")
	page := readAsset(t, "app.js")
	const route = `const ROUTE = /^#\/(?:s\/[^/?#]+(?:\/r\/[^/?#]+)?)?$/;`
	for name, script := range map[string]string{"sw.js": sw, "app.js": page} {
		if !strings.Contains(script, route) {
			t.Errorf("%s does not narrow a route to the page's forms", name)
		}
	}
	for _, want := range []string{
		`client.postMessage({ type: "tmag-route", route })`,
		`try { await client.focus(); } catch {`,
		`clients.openWindow("/?open=" + encodeURIComponent(route))`,
	} {
		if !strings.Contains(sw, want) {
			t.Errorf("sw.js: no %s", want)
		}
	}
	posted := strings.Index(sw, `client.postMessage({ type: "tmag-route", route })`)
	focused := strings.Index(sw, `client.focus()`)
	if posted < 0 || focused < 0 || posted > focused {
		t.Error("sw.js posts the route after focus can fail")
	}
	if strings.Contains(sw, "client.navigate") {
		t.Error("sw.js still relies on WebKit's navigate")
	}
	for _, want := range []string{
		`navigator.serviceWorker.addEventListener("message"`,
		`event.data.type === "tmag-route"`,
		`params.get("open")`,
		`params.delete("open")`,
		`history.replaceState(null, "", location.pathname`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("app.js: no %s", want)
		}
	}
}

// A listener alone does not start the messages WebKit queued for the page.
func TestServiceWorkerMessagesStartOnLoad(t *testing.T) {
	page := readAsset(t, "app.js")
	listener := strings.Index(page, `navigator.serviceWorker.addEventListener("message"`)
	start := strings.Index(page, `navigator.serviceWorker.startMessages()`)
	if listener < 0 || start < 0 || start < listener {
		t.Fatal("app.js does not start the queued service worker messages after listening")
	}
}

// A tap leaves its route in IndexedDB for a page that reloads or finds no
// message.
func TestNotificationTapLeavesItsRouteForThePage(t *testing.T) {
	sw := readAsset(t, "sw.js")
	page := readAsset(t, "app.js")
	for name, script := range map[string]string{"sw.js": sw, "app.js": page} {
		for _, want := range []string{
			`const ROUTE_DB = "tmag-route";`,
			`const ROUTE_STORE = "route";`,
			`const ROUTE_KEY = "tapped";`,
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: no %s", name, want)
			}
		}
	}
	for _, want := range []string{
		`indexedDB.open(ROUTE_DB, DB_VERSION)`,
		`put({ route, at: Date.now() }, ROUTE_KEY)`,
		`await rememberRoute(route)`,
	} {
		if !strings.Contains(sw, want) {
			t.Errorf("sw.js: no %s", want)
		}
	}
	stored := strings.Index(sw, `await rememberRoute(route)`)
	posted := strings.Index(sw, `client.postMessage({ type: "tmag-route", route })`)
	if stored < 0 || posted < 0 || stored > posted {
		t.Error("sw.js posts the route before keeping it for the page")
	}
	for _, want := range []string{
		`const ROUTE_FRESH = 2 * 60 * 1000;`,
		`store.get(ROUTE_KEY)`,
		`store.delete(ROUTE_KEY)`,
		`validRoute(record.route)`,
		`Date.now() - record.at < ROUTE_FRESH`,
		`const tapped = await takeTappedRoute();`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("app.js: no %s", want)
		}
	}
}

// A push keeps its alert for a page whose app never fires a click.
func TestPushKeepsRecentAlertsForThePage(t *testing.T) {
	sw := readAsset(t, "sw.js")
	page := readAsset(t, "app.js")
	for name, script := range map[string]string{"sw.js": sw, "app.js": page} {
		for _, want := range []string{
			`const ALERTS_STORE = "alerts";`,
			`const ALERT_TTL = 10 * 60 * 1000;`,
			`const DB_VERSION = 2;`,
		} {
			if !strings.Contains(script, want) {
				t.Errorf("%s: no %s", name, want)
			}
		}
	}
	for _, want := range []string{
		`const ALERT_LIMIT = 20;`,
		`rememberAlert({ tag: options.tag, route, at: Date.now() })`,
		`store.put(alert)`,
		`recent.slice(0, ALERT_LIMIT)`,
	} {
		if !strings.Contains(sw, want) {
			t.Errorf("sw.js: no %s", want)
		}
	}
	remembered := strings.Index(sw, `await rememberAlert({ tag: options.tag, route, at: Date.now() })`)
	shown := strings.Index(sw, `await self.registration.showNotification(title, options)`)
	if remembered < 0 || shown < 0 || remembered > shown {
		t.Error("sw.js shows an alert before keeping it for the page")
	}
	for _, want := range []string{
		`registration.getNotifications()`,
		`typeof registration.getNotifications !== "function"`,
		`const displayed = new Set((shown || []).map(notification => notification.tag || ""));`,
		`const taken = recent.filter(alert => !displayed.has(alert.tag));`,
		`forgetAlerts(taken.map(alert => alert.tag))`,
		`validRoute(alert.route)`,
		`await takeAlertRoute(!opened && !tapped);`,
		`await takeAlertRoute(!tapped);`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("app.js: no %s", want)
		}
	}
}

// A home-screen web app keeps its own storage, so iOS pairs from there.
func TestPairingOnIOSStartsFromHomeScreen(t *testing.T) {
	page := readAsset(t, "app.js")
	if !strings.Contains(page, `IOS && !STANDALONE ? el("p", { class: "note", text: "On iOS, add this page to the home screen first, then pair from there: the home-screen app keeps its own storage." }) : null`) {
		t.Fatal("the pairing view does not send iOS to the home screen first")
	}
}

// The worker caches nothing, so a new one takes over before every iOS window
// closes.
func TestNewWorkerTakesOverAtOnce(t *testing.T) {
	sw := readAsset(t, "sw.js")
	for _, want := range []string{
		`self.addEventListener("install", event => {`,
		`event.waitUntil(self.skipWaiting());`,
		`self.addEventListener("activate", event => {`,
		`event.waitUntil(clients.claim());`,
	} {
		if !strings.Contains(sw, want) {
			t.Errorf("sw.js: no %s", want)
		}
	}
}
