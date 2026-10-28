package doctor

import (
	"context"
	"errors"
	"fmt"
	"github.com/rezzminator/harvester/internal/config"
	"github.com/rezzminator/harvester/internal/deps"
	"github.com/rezzminator/harvester/internal/harvest"
	"github.com/rezzminator/harvester/internal/harvestpy"
	"github.com/rezzminator/harvester/internal/obs"
	"github.com/rezzminator/harvester/internal/paths"
	"io"
	"os"
	"path/filepath"
	goRuntime "runtime"
	"strings"
)

const brokenState = "broken"
const StateUnknown = "UNKNOWN"

type harvestDoctor interface {
	Inspect(string, harvestpy.Platform) (harvestpy.EnvironmentDigest, error)
	Check(context.Context, string, harvestpy.Platform) (harvestpy.CheckReport, error)
}

var HarvestOverride harvestDoctor

func printHarvestCacheDoctor(stdout io.Writer, harvester config.HarvesterConfig) int {
	root, rootErr := harvest.CacheRoot(harvester.Cache.Dir)
	if rootErr != nil {
		// An unresolvable root is "we failed to look", never "0 entries".
		fmt.Fprintf(stdout, "doctor: harvester_cache dir=? error=%v\n", rootErr)
		return 1
	}
	entries := 0
	walkErr := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			entries++
		}
		return nil
	})
	if os.IsNotExist(walkErr) {
		walkErr = nil
	}
	ttlText := harvester.Cache.TTL.String()
	if harvester.Cache.TTL == 0 {
		ttlText = "never"
	}
	if walkErr != nil {
		fmt.Fprintf(
			stdout,
			"doctor: harvester_cache dir=%s entries=%d ttl=%s error=%v\n",
			root,
			entries,
			ttlText,
			walkErr,
		)
		return 1
	}
	fmt.Fprintf(stdout, "doctor: harvester_cache dir=%s entries=%d ttl=%s\n", root, entries, ttlText)
	return 0
}

// printHarvestSearchDoctor prints the search tool's one health line; the
// probe itself (OFF/reachable/UNREACHABLE/configured classification) lives in
// harvest.ProbeSearch, never here — cmd/harvester is dispatch, not policy.
func printHarvestSearchDoctor(ctx context.Context, stdout io.Writer, harvester config.HarvesterConfig) int {
	probe := harvest.ProbeSearch(ctx, harvest.SearchOptions{
		SearXNGURL:    harvester.Search.SearXNGURL,
		BraveAPIKey:   harvester.Search.BraveAPIKey,
		DisableSearch: !harvester.Search.Enabled,
	}, nil)
	fmt.Fprintf(
		stdout,
		"doctor: harvester search state=%s backend=%s detail=%s\n",
		probe.State,
		probe.Backend,
		probe.Detail,
	)
	if probe.Warning {
		return 1
	}
	return 0
}

func configuredHarvestDoctor() harvestDoctor {
	if HarvestOverride != nil {
		return HarvestOverride
	}
	return pinnedHarvestDoctor{}
}

func printHarvestPythonDoctorWithRunner(
	ctx context.Context,
	stdout io.Writer,
	home string,
	platform harvestpy.Platform,
	doctor harvestDoctor,
	browserGate bool,
	runner deps.Runner,
) int {
	if runner == nil {
		runner = obs.Runner(deps.RealRunner{})
	}
	if pinned, ok := doctor.(pinnedHarvestDoctor); ok {
		pinned.runner = runner
		doctor = pinned
	}
	if platform.GOOS == "" {
		platform.GOOS, platform.GOARCH = goRuntime.GOOS, goRuntime.GOARCH
	}
	root := paths.PythonRoot(home)
	if override := os.Getenv("HARVESTER_PYTHON_ROOT"); override != "" {
		root = override
	}
	current := harvestpy.RuntimeRoot(root, platform)
	interpreter := filepath.Join(current, "project", ".venv", "bin", "python")
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "doctor: harvestpy skipped")
		// The conversion env is absent (honest absence), but the opt-in
		// browser row must still report: with fetch.browser on a missing
		// environment is the NOT_PROVISIONED state, never silence.
		return appendHarvestBrowserDoctorRowWithRunner(
			ctx,
			stdout,
			root,
			platform,
			0,
			browserGate,
			runner,
			func() string {
				return resolveChromeForDoctorWithRunner(runner)
			},
		)
	}
	warnings := 0

	plan, planErr := harvestpy.PlanConversionEnvironment(platform)
	if planErr != nil {
		fmt.Fprintf(stdout, "doctor: harvestpy pinned_version=(default) unavailable error=%v\n", planErr)
		warnings++
	} else {
		fmt.Fprintf(stdout, "doctor: harvestpy pinned_version=(default) %s\n", plan.PythonVersion)
		if len(plan.PackageBlockers) != 0 || plan.PackageDownloadStatus == "blocked-exact-lock" {
			warnings++
			fmt.Fprintf(stdout, "doctor: harvestpy package_plan=(default) blocked-exact-lock\n")
			for _, blocker := range plan.PackageBlockers {
				fmt.Fprintf(stdout, "doctor: harvestpy package_blocker=%s\n", blocker)
			}
		}
	}

	digest, inspectErr := doctor.Inspect(root, platform)
	report, checkErr := doctor.Check(ctx, root, platform)
	if report.Digest.Python != "" {
		digest = report.Digest
	}

	interpreterStatus, interpreterErr := harvestDoctorCheck(report, "interpreter", checkErr)
	if interpreterStatus {
		fmt.Fprintf(stdout, "doctor: harvestpy interpreter=(file) %s version=%s\n", interpreter, digest.Python)
	} else {
		warnings++
		fmt.Fprintf(stdout, "doctor: harvestpy interpreter=(file) %s broken error=%s\n", interpreter, interpreterErr)
	}
	if inspectErr != nil && digest.LockSHA256 == "" {
		warnings++
		fmt.Fprintf(stdout, "doctor: harvestpy marker=(file) broken error=%v\n", inspectErr)
	}

	// "incomplete" is the word reserved for a marker left by an interrupted
	// provision (digest.State == "incomplete"); any other lock/inventory
	// failure is a live check failure against a provision that DID finish —
	// a decode error, a stale lock, a dependency drift — and that is
	// "broken", never "incomplete".
	harvestFailureWord := brokenState
	if digest.State == "incomplete" {
		harvestFailureWord = "incomplete"
	}

	lockOK, lockErr := harvestDoctorCheck(report, "lock_hash", checkErr)
	if lockOK && digest.LockSHA256 != "" {
		fmt.Fprintf(stdout, "doctor: harvestpy lock=(file) complete digest=%s\n", digest.LockSHA256)
	} else {
		warnings++
		if lockErr == "" {
			lockErr = "lock digest or completeness check is missing"
		}
		fmt.Fprintf(stdout, "doctor: harvestpy lock=(file) %s error=%s\n", harvestFailureWord, lockErr)
	}

	inventoryOK, inventoryErr := harvestDoctorCheck(report, "lock_completeness", checkErr)
	if inventoryOK && digest.InventorySHA256 != "" && digest.InventoryCount > 0 {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy inventory=(file) complete count=%d digest=%s\n",
			digest.InventoryCount,
			digest.InventorySHA256,
		)
	} else {
		warnings++
		if inventoryErr == "" {
			inventoryErr = "installed inventory digest/count is missing"
		}
		fmt.Fprintf(stdout, "doctor: harvestpy inventory=(file) %s error=%s\n", harvestFailureWord, inventoryErr)
	}

	smokeOK, smokeErr := harvestDoctorCheck(report, "live_smoke", checkErr)
	conversionOK, conversionErr := harvestDoctorCheck(report, "live_smoke_conversion", checkErr)
	if smokeOK && conversionOK {
		fmt.Fprintln(stdout, "doctor: harvestpy live_smoke=(file) healthy")
	} else {
		warnings++
		if smokeErr == "" {
			smokeErr = conversionErr
		}
		if smokeErr == "" {
			smokeErr = "live smoke check did not pass"
		}
		fmt.Fprintf(stdout, "doctor: harvestpy live_smoke=(file) broken error=%s\n", smokeErr)
	}
	warnings += printUnnamedHarvestChecks(stdout, report, harvestNamedDoctorChecks)
	return appendHarvestBrowserDoctorRowWithRunner(
		ctx,
		stdout,
		root,
		platform,
		warnings,
		browserGate,
		runner,
		func() string {
			return resolveChromeForDoctorWithRunner(runner)
		},
	)
}

// resolveChromeForDoctor re-checks the Google Chrome locations the browser
// worker itself resolves (browser.py CHROME_CANDIDATES), without launching
// anything. Chromium variants are deliberately absent: channel="chrome"
// launches only GOOGLE Chrome, so reporting a chromium-only host as healthy
// would pass smoke and fail every launch.
func resolveChromeForDoctor() string {
	return resolveChromeForDoctorWithRunner(obs.Runner(deps.RealRunner{}))
}

func resolveChromeForDoctorWithRunner(runner deps.Runner) string {
	if runner == nil {
		runner = obs.Runner(deps.RealRunner{})
	}
	for _, candidate := range []string{
		"google-chrome", "google-chrome-stable",
		"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	} {
		if strings.ContainsRune(candidate, filepath.Separator) {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
				return candidate
			}
			continue
		}
		if path, err := runner.LookPath(candidate); err == nil {
			return path
		}
	}
	return ""
}

// appendHarvestBrowserDoctorRow reports the opt-in real-browser rung
// (Patchright + system Chrome). Its broken states are deliberately distinct:
// NOT provisioned ≠ provisioned-but-Chrome-missing ≠ probe failed. With the
// fetch.browser gate off the row is informational only — absence of an
// opt-in environment is not a defect.
// doctorChromeResolver is injectable so tests can simulate a Chrome-less
// host without depending on the machine they run on.
var doctorChromeResolver = resolveChromeForDoctor

func browserEnvFingerprint(digest harvestpy.EnvironmentDigest) string {
	if len(digest.Digest) >= 8 {
		return digest.Digest[:8]
	}
	return StateUnknown
}

// gateOn is harvester.config.json fetch.browser — the same value the core's
// ladder receives, so doctor and the harvester can never disagree.
func appendHarvestBrowserDoctorRow(
	ctx context.Context,
	stdout io.Writer,
	root string,
	platform harvestpy.Platform,
	gateOn bool,
) int {
	return appendHarvestBrowserDoctorRowWithRunner(
		ctx,
		stdout,
		root,
		platform,
		0,
		gateOn,
		obs.Runner(deps.RealRunner{}),
		doctorChromeResolver,
	)
}

func appendHarvestBrowserDoctorRowWithRunner(
	ctx context.Context,
	stdout io.Writer,
	root string,
	platform harvestpy.Platform,
	warnings int,
	gateOn bool,
	runner deps.Runner,
	chromeResolver func() string,
) int {
	if runner == nil {
		runner = obs.Runner(deps.RealRunner{})
	}
	if chromeResolver == nil {
		chromeResolver = func() string { return resolveChromeForDoctorWithRunner(runner) }
	}
	digest, inspectErr := harvestpy.InspectBrowser(root, platform)
	envDir := harvestpy.BrowserRuntimeRoot(root, platform)
	interpreter := filepath.Join(envDir, "project", ".venv", "bin", "python")
	script := filepath.Join(envDir, "project", "browser.py")
	fingerprint := "env=UNKNOWN"
	if inspectErr == nil {
		fingerprint = fmt.Sprintf("env=%s", browserEnvFingerprint(digest))
	}
	// S7: the gate-off row stays informational, but its states NEVER
	// collapse — never-provisioned, provisioned, and corrupt-record are
	// three different answers even while disabled.
	if !gateOn {
		switch {
		case inspectErr == nil && digest.State == "ready":
			fmt.Fprintf(stdout, "doctor: harvestpy_browser %s provisioned disabled gate=fetch.browser\n", fingerprint)
		case errors.Is(inspectErr, os.ErrNotExist):
			fmt.Fprintf(
				stdout,
				"doctor: harvestpy_browser env=NOT_PROVISIONED disabled gate=fetch.browser hint=set fetch.browser=true in harvester.config.json; the next browser fetch provisions it (needs uv and network access)\n",
			)
		case inspectErr != nil:
			fmt.Fprintf(
				stdout,
				"doctor: harvestpy_browser env=CORRUPT_RECORD disabled gate=fetch.browser error=%v\n",
				inspectErr,
			)
		default:
			fmt.Fprintf(
				stdout,
				"doctor: harvestpy_browser %s disabled gate=fetch.browser error=provision record state %q is not ready\n",
				fingerprint,
				digest.State,
			)
		}
		return warnings
	}
	if inspectErr != nil {
		if errors.Is(inspectErr, os.ErrNotExist) {
			fmt.Fprintf(
				stdout,
				"doctor: harvestpy_browser env=NOT_PROVISIONED interpreter=%s error=browser environment was never provisioned hint=fetch.browser is already true; the next fetch that reaches the browser rung provisions it (needs uv and network access)\n",
				interpreter,
			)
		} else {
			fmt.Fprintf(stdout, "doctor: harvestpy_browser env=PROBE_FAILED error=%v\n", inspectErr)
		}
		return warnings + 1
	}
	if digest.State != "ready" {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser env=PROBE_FAILED state=%s error=browser environment record is not ready\n",
			digest.State,
		)
		return warnings + 1
	}
	if _, statErr := os.Stat(interpreter); statErr != nil {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser %s interpreter=%s PROBE_FAILED error=%v\n",
			fingerprint,
			interpreter,
			statErr,
		)
		return warnings + 1
	}
	// S2: verify the on-disk WORKER against the provision record before
	// anything else — browser.py carries the SSRF route guard, and a file
	// that does not match the pinned source invalidates every verdict below.
	if strings.TrimSpace(digest.SourceSHA256) == "" {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser %s SOURCE_UNPINNED error=provision record predates source pinning; remove the browser environment so the next browser fetch re-provisions it\n",
			fingerprint,
		)
		return warnings + 1
	}
	if err := harvestpy.VerifySHA256(script, digest.SourceSHA256); err != nil {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser %s SOURCE_MISMATCH error=on-disk browser.py does not match the pinned provision source (the SSRF route guard cannot be trusted): %v\n",
			fingerprint,
			err,
		)
		return warnings + 1
	}
	// S2: LIVE smoke — patchright importability and Chrome resolution are
	// proven NOW, on this host, exactly as a fetch would; the provision-time
	// record alone is a snapshot, and snapshots go stale silently.
	smoke, smokeErr := runDoctorBrowserSmokeWithRunner(ctx, interpreter, script, runner)
	if smokeErr != nil {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser %s BROKEN_SMOKE error=live worker smoke failed: %v\n",
			fingerprint,
			smokeErr,
		)
		return warnings + 1
	}
	patchrightLive, _ := smoke["patchright"].(bool)
	if ok, _ := smoke["ok"].(bool); !ok || !patchrightLive {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser %s patchright=MISSING BROKEN_SMOKE error=live smoke reports patchright did not import\n",
			fingerprint,
		)
		return warnings + 1
	}
	liveChrome, _ := smoke["chrome_path"].(string)
	chromePath, _ := digest.Imports["chrome_path"].(string)
	// Prefer the live resolution; fall back to re-verifying the recorded
	// path, then to a fresh host scan, before declaring Chrome missing.
	for _, candidate := range []string{strings.TrimSpace(liveChrome), strings.TrimSpace(chromePath)} {
		if candidate == "" || strings.ContainsRune(candidate, filepath.Separator) {
			if info, statErr := os.Stat(candidate); candidate != "" && statErr == nil && info.Mode().IsRegular() {
				fmt.Fprintf(
					stdout,
					"doctor: harvestpy_browser %s patchright=present(live smoke) chrome=%s healthy source_hash=%s\n",
					fingerprint,
					candidate,
					harvestpy.BrowserSourceState(digest),
				)
				return warnings
			}
			continue
		}
		if _, lookErr := runner.LookPath(candidate); lookErr == nil {
			fmt.Fprintf(
				stdout,
				"doctor: harvestpy_browser %s patchright=present(live smoke) chrome=%s healthy source_hash=%s\n",
				fingerprint,
				candidate,
				harvestpy.BrowserSourceState(digest),
			)
			return warnings
		}
	}
	if fallback := chromeResolver(); fallback != "" {
		fmt.Fprintf(
			stdout,
			"doctor: harvestpy_browser %s patchright=present(live smoke) chrome=%s healthy source_hash=%s\n",
			fingerprint,
			fallback,
			harvestpy.BrowserSourceState(digest),
		)
		return warnings
	}
	fmt.Fprintf(
		stdout,
		"doctor: harvestpy_browser %s patchright=present(live smoke) chrome=MISSING error=environment provisioned but no system Chrome binary resolves\n",
		fingerprint,
	)
	return warnings + 1
}

func harvestDoctorCheck(report harvestpy.CheckReport, name string, checkErr error) (bool, string) {
	if status, ok := report.Checks[name]; ok {
		return status.OK, status.Error
	}
	if report.Healthy && checkErr == nil {
		return true, ""
	}
	if checkErr != nil {
		return false, checkErr.Error()
	}
	return false, "check did not report healthy"
}
