package auth

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type BrowserAuth struct {
	debug     bool
	tempDir   string
	chromeCmd *exec.Cmd
	cancel    context.CancelFunc
	useExec   bool
}

func New(debug bool) *BrowserAuth {
	return &BrowserAuth{
		debug:   debug,
		useExec: false,
	}
}

type Options struct {
	ProfileName string
}

type Option func(*Options)

func WithProfileName(p string) Option { return func(o *Options) { o.ProfileName = p } }

func (ba *BrowserAuth) GetAuth(opts ...Option) (token, cookies string, err error) {
	o := &Options{
		ProfileName: "Default",
	}
	for _, opt := range opts {
		opt(o)
	}

	defer ba.cleanup()

	// Create temp directory for new Chrome instance
	if ba.debug {
	}
	tempDir, err := os.MkdirTemp("", "nlm-chrome-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp dir: %w", err)
	}
	ba.tempDir = tempDir

	// Copy profile data
	if err := ba.copyProfileData(o.ProfileName); err != nil {
		return "", "", fmt.Errorf("copy profile: %w", err)
	}

	var ctx context.Context
	var cancel context.CancelFunc
	var debugURL string

	if ba.useExec {
		// Use original exec.Command approach
		debugURL, err = ba.startChromeExec()
		if err != nil {
			return "", "", err
		}
		allocCtx, allocCancel := chromedp.NewRemoteAllocator(context.Background(), debugURL)
		ba.cancel = allocCancel
		ctx, cancel = chromedp.NewContext(allocCtx)
	} else {
		// Use chromedp.ExecAllocator approach
		opts := []chromedp.ExecAllocatorOption{
			chromedp.NoFirstRun,
			chromedp.NoDefaultBrowserCheck,
			chromedp.DisableGPU,
			chromedp.Flag("disable-extensions", true),
			chromedp.Flag("disable-sync", true),
			chromedp.Flag("disable-popup-blocking", true),
			chromedp.Flag("window-size", "1280,800"),
			chromedp.UserDataDir(ba.tempDir),
			chromedp.Flag("headless", !ba.debug),
			chromedp.Flag("disable-hang-monitor", true),
			chromedp.Flag("disable-ipc-flooding-protection", true),
			chromedp.Flag("disable-popup-blocking", true),
			chromedp.Flag("disable-prompt-on-repost", true),
			chromedp.Flag("disable-renderer-backgrounding", true),
			chromedp.Flag("disable-sync", true),
			chromedp.Flag("force-color-profile", "srgb"),
			chromedp.Flag("metrics-recording-only", true),
			chromedp.Flag("safebrowsing-disable-auto-update", true),
			chromedp.Flag("enable-automation", true),
			chromedp.Flag("password-store", "basic"),
		}

		allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
		ba.cancel = allocCancel
		ctx, cancel = chromedp.NewContext(allocCtx)
	}
	defer cancel()

	// Allow ample time for the user to complete any interactive
	// authentication flows in the browser before we give up.  The
	// previous 60s deadline was too short and caused "context deadline
	// exceeded" errors if the user took longer to sign in, so increase it
	// to five minutes.
	ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if ba.debug {
		ctx, _ = chromedp.NewContext(ctx, chromedp.WithLogf(func(format string, args ...interface{}) {
			fmt.Printf("ChromeDP: "+format+"\n", args...)
		}))
	}

	return ba.extractAuthData(ctx)
}

func (ba *BrowserAuth) copyProfileData(profileName string) error {
	sourceDir := filepath.Join(getProfilePath(), profileName)
	if ba.debug {
		fmt.Printf("Copying profile data from: %s\n", sourceDir)
	}

	// Create Default profile directory
	defaultDir := filepath.Join(ba.tempDir, "Default")
	if err := os.MkdirAll(defaultDir, 0755); err != nil {
		return fmt.Errorf("create profile dir: %w", err)
	}

	// Copy essential files
	files := []string{
		"Cookies",
		"Login Data",
		"Web Data",
	}

	for _, file := range files {
		src := filepath.Join(sourceDir, file)
		dst := filepath.Join(defaultDir, file)

		if err := copyFile(src, dst); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("issue with profile copy %s: %w", file, err)
			}
			if ba.debug {
				fmt.Printf("Skipping non-existent file: %s\n", file)
			}
		}
	}

	// explain each of these lines in a comment (explain why:)
	// Create minimal Local State file
	localState := `{"os_crypt":{"encrypted_key":""}}`
	if err := os.WriteFile(filepath.Join(ba.tempDir, "Local State"), []byte(localState), 0644); err != nil {
		return fmt.Errorf("write local state: %w", err)
	}

	return nil
}

func (ba *BrowserAuth) startChromeExec() (string, error) {
	debugPort := "9222"
	debugURL := fmt.Sprintf("http://localhost:%s", debugPort)

	chromePath := getChromePath()
	if chromePath == "" {
		return "", fmt.Errorf("chrome not found")
	}

	if ba.debug {
		fmt.Printf("Starting Chrome from: %s\n", chromePath)
		fmt.Printf("Using profile: %s\n", ba.tempDir)
	}

	ba.chromeCmd = exec.Command(chromePath,
		fmt.Sprintf("--remote-debugging-port=%s", debugPort),
		fmt.Sprintf("--user-data-dir=%s", ba.tempDir),
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-sync",
		"--window-size=1280,800",
	)

	if ba.debug {
		ba.chromeCmd.Stdout = os.Stdout
		ba.chromeCmd.Stderr = os.Stderr
	}

	if err := ba.chromeCmd.Start(); err != nil {
		return "", fmt.Errorf("start chrome: %w", err)
	}

	if err := ba.waitForDebugger(debugURL); err != nil {
		ba.cleanup()
		return "", err
	}

	return debugURL, nil
}

func (ba *BrowserAuth) waitForDebugger(debugURL string) error {
	fmt.Println("Waiting for Chrome debugger...")

	timeout := time.After(20 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout waiting for chrome debugger")
		case <-ticker.C:
			resp, err := http.Get(debugURL + "/json/version")
			if err == nil {
				resp.Body.Close()
				fmt.Println("Chrome debugger ready")
				return nil
			}
			if ba.debug {
				fmt.Printf(".")
			}
		}
	}
}

func (ba *BrowserAuth) cleanup() {
	if ba.cancel != nil {
		ba.cancel()
	}
	if ba.chromeCmd != nil && ba.chromeCmd.Process != nil {
		ba.chromeCmd.Process.Kill()
	}
	if ba.tempDir != "" {
		os.RemoveAll(ba.tempDir)
	}
}

func copyFile(src, dst string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()

	_, err = io.Copy(destination, source)
	return err
}

func (ba *BrowserAuth) extractAuthData(ctx context.Context) (token, cookies string, err error) {
	// Navigate and wait for initial page load
	if err := chromedp.Run(ctx,
		chromedp.Navigate("https://notebooklm.google.com"),
		chromedp.WaitVisible("body", chromedp.ByQuery),
	); err != nil {
		return "", "", fmt.Errorf("failed to load page: %w", err)
	}

	// After the page loads we poll for authentication data.  Give the user
	// several minutes here as well since the initial load may redirect
	// through multiple login screens.  This keeps the overall timeout under
	// the 5 minute limit above but provides a lot more breathing room than
	// the previous 30s window.
	pollCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-pollCtx.Done():
			var currentURL string
			_ = chromedp.Run(ctx, chromedp.Location(&currentURL))
			return "", "", fmt.Errorf("auth data not found after timeout (URL: %s)", currentURL)

		case <-ticker.C:
			token, cookies, err = ba.tryExtractAuth(ctx)
			if err != nil {
				if ba.debug {
					// show seconds remaining from ctx at end of this:
					deadline, _ := ctx.Deadline()
					remaining := time.Until(deadline).Seconds()
					fmt.Printf("   Auth check failed: %v (%.1f seconds remaining)\n", err, remaining)
				}
				continue
			}
			if token != "" {
				return token, cookies, nil
			}
			if ba.debug {
				fmt.Println("Waiting for auth data...")
			}
		}
	}
}

func (ba *BrowserAuth) tryExtractAuth(ctx context.Context) (token, cookies string, err error) {
	var hasAuth bool
	err = chromedp.Run(ctx,
		chromedp.Evaluate(`!!window.WIZ_global_data`, &hasAuth),
	)
	if err != nil {
		return "", "", fmt.Errorf("check auth presence: %w", err)
	}

	if !hasAuth {
		return "", "", nil
	}

	err = chromedp.Run(ctx,
		chromedp.Evaluate(`WIZ_global_data.SNlM0e`, &token),
		chromedp.ActionFunc(func(ctx context.Context) error {
			cks, err := network.GetCookies().WithUrls([]string{"https://notebooklm.google.com"}).Do(ctx)
			if err != nil {
				return fmt.Errorf("get cookies: %w", err)
			}

			var cookieStrs []string
			for _, ck := range cks {
				cookieStrs = append(cookieStrs, fmt.Sprintf("%s=%s", ck.Name, ck.Value))
			}
			cookies = strings.Join(cookieStrs, "; ")
			return nil
		}),
	)
	if err != nil {
		return "", "", fmt.Errorf("extract auth data: %w", err)
	}

	return token, cookies, nil
}
