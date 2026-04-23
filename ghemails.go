package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Rate limit: 1 request per 10 seconds for api.github.com calls
const rateLimitDelay = 10 * time.Second

// Number of concurrent workers for fetching raw file contents from
// raw.githubusercontent.com (separate endpoint, not rate-limited).
const fetchWorkers = 5

// GitHub Search API endpoints
const (
	codeSearchURL    = "https://api.github.com/search/code"
	commitsSearchURL = "https://api.github.com/search/commits"
)

// CodeSearchResponse represents the GitHub code search API response
type CodeSearchResponse struct {
	TotalCount        int  `json:"total_count"`
	IncompleteResults bool `json:"incomplete_results"`
	Items             []struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		HTMLURL    string `json:"html_url"`
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
		TextMatches []struct {
			Fragment string `json:"fragment"`
		} `json:"text_matches"`
	} `json:"items"`
	Message string `json:"message,omitempty"`
}

// CommitsSearchResponse represents the GitHub commits search API response
type CommitsSearchResponse struct {
	TotalCount        int  `json:"total_count"`
	IncompleteResults bool `json:"incomplete_results"`
	Items             []struct {
		SHA     string `json:"sha"`
		HTMLURL string `json:"html_url"`
		Commit  struct {
			Author struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"committer"`
			Message string `json:"message"`
		} `json:"commit"`
		Repository struct {
			FullName string `json:"full_name"`
			HTMLURL  string `json:"html_url"`
		} `json:"repository"`
	} `json:"items"`
	Message string `json:"message,omitempty"`
}

// EmailInfo tracks where each email was found
type EmailInfo struct {
	Email   string
	Sources map[string]bool // unique sources (URLs)
}

// Finder holds the search state
type Finder struct {
	domain     string
	token      string
	client     *http.Client
	emails     map[string]*EmailInfo
	emailRegex *regexp.Regexp
	lastReq    time.Time
	mu         sync.Mutex // protects emails map during concurrent fetches
	rateMu     sync.Mutex // serializes rateLimit() so it can't be bypassed by parallel callers
}

func NewFinder(domain, token string) *Finder {
	// Regex matches any email at the target domain (case-insensitive)
	pattern := fmt.Sprintf(`(?i)[a-zA-Z0-9._%%+-]+@%s`, regexp.QuoteMeta(domain))
	return &Finder{
		domain:     domain,
		token:      token,
		client:     &http.Client{Timeout: 30 * time.Second},
		emails:     make(map[string]*EmailInfo),
		emailRegex: regexp.MustCompile(pattern),
	}
}

// rateLimit enforces 1 request per 10 seconds across all callers.
// Only applies to api.github.com calls; raw.githubusercontent.com fetches bypass this.
func (f *Finder) rateLimit() {
	f.rateMu.Lock()
	defer f.rateMu.Unlock()
	elapsed := time.Since(f.lastReq)
	if elapsed < rateLimitDelay {
		wait := rateLimitDelay - elapsed
		fmt.Printf("[*] Waiting %.1fs before next request...\n", wait.Seconds())
		time.Sleep(wait)
	}
	f.lastReq = time.Now()
}

// doRequest performs an authenticated GitHub API request
func (f *Finder) doRequest(endpoint, accept string) ([]byte, int, error) {
	f.rateLimit()

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, 0, err
	}

	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "go-github-email-finder")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	// Handle secondary rate-limit hints from GitHub
	if resp.StatusCode == 403 || resp.StatusCode == 429 {
		retryAfter := resp.Header.Get("Retry-After")
		fmt.Printf("[!] Rate limited (HTTP %d). Retry-After: %s\n", resp.StatusCode, retryAfter)
	}

	return body, resp.StatusCode, nil
}

// addEmail records a found email with its source (thread-safe)
func (f *Finder) addEmail(email, source string) {
	email = strings.ToLower(email)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.emails[email]; !ok {
		f.emails[email] = &EmailInfo{
			Email:   email,
			Sources: make(map[string]bool),
		}
		fmt.Printf("[+] Found: %s  (from %s)\n", email, source)
	}
	f.emails[email].Sources[source] = true
}

// searchCode searches GitHub code for @domain, then fetches each matching
// file's raw contents and extracts every email at the target domain.
func (f *Finder) searchCode() {
	fmt.Printf("\n[*] Searching GitHub CODE for @%s ...\n", f.domain)

	query := fmt.Sprintf("\"@%s\"", f.domain)
	perPage := 100

	// Collect all matching files first so we can dedupe before fetching raw content.
	// Key: "owner/repo|path" -> raw content URL. This avoids re-downloading the
	// same file if it shows up on multiple search pages.
	files := make(map[string]string)

	// GitHub's search API caps results at 1000 total (10 pages * 100)
	for page := 1; page <= 10; page++ {
		params := url.Values{}
		params.Set("q", query)
		params.Set("per_page", fmt.Sprintf("%d", perPage))
		params.Set("page", fmt.Sprintf("%d", page))

		endpoint := codeSearchURL + "?" + params.Encode()
		body, status, err := f.doRequest(endpoint, "application/vnd.github+json")
		if err != nil {
			fmt.Printf("[!] code search error: %v\n", err)
			return
		}

		if status != 200 {
			fmt.Printf("[!] code search HTTP %d: %s\n", status, truncate(string(body), 300))
			return
		}

		var resp CodeSearchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			fmt.Printf("[!] code search parse error: %v\n", err)
			return
		}

		if page == 1 {
			fmt.Printf("[*] Code total matches reported: %d (will fetch and scan each file)\n", resp.TotalCount)
		}

		if len(resp.Items) == 0 {
			break
		}

		for _, item := range resp.Items {
			key := item.Repository.FullName + "|" + item.Path
			if _, seen := files[key]; !seen {
				// Convert the blob HTML URL into a raw.githubusercontent.com URL.
				// item.HTMLURL looks like: https://github.com/<owner>/<repo>/blob/<ref>/<path>
				raw := strings.Replace(item.HTMLURL, "https://github.com/", "https://raw.githubusercontent.com/", 1)
				raw = strings.Replace(raw, "/blob/", "/", 1)
				files[key] = raw
			}
		}

		if len(resp.Items) < perPage {
			break
		}
	}

	fmt.Printf("[*] Unique files to fetch: %d (using %d workers)\n", len(files), fetchWorkers)

	// Fetch and scan files concurrently with a bounded worker pool.
	type job struct {
		key    string
		rawURL string
	}
	jobs := make(chan job, len(files))
	for key, rawURL := range files {
		jobs <- job{key, rawURL}
	}
	close(jobs)

	var wg sync.WaitGroup
	var counter int64
	var counterMu sync.Mutex
	total := len(files)

	for w := 0; w < fetchWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := range jobs {
				counterMu.Lock()
				counter++
				i := counter
				counterMu.Unlock()

				fmt.Printf("[*] (%d/%d) [w%d] Fetching: %s\n", i, total, workerID, j.key)
				content, err := f.fetchRaw(j.rawURL)
				if err != nil {
					fmt.Printf("[!] fetch error: %v\n", err)
					continue
				}

				// Reconstruct the blob URL from the raw URL for user-friendly reporting.
				// raw URL: https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>
				// blob URL: https://github.com/<owner>/<repo>/blob/<ref>/<path>
				blobURL := j.rawURL
				rest := strings.TrimPrefix(j.rawURL, "https://raw.githubusercontent.com/")
				parts := strings.SplitN(rest, "/", 4)
				if len(parts) == 4 {
					blobURL = fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s",
						parts[0], parts[1], parts[2], parts[3])
				}

				for _, m := range f.emailRegex.FindAllString(content, -1) {
					f.addEmail(m, blobURL)
				}
			}
		}(w + 1)
	}
	wg.Wait()
}

// fetchRaw downloads a file from raw.githubusercontent.com.
// NOT rate-limited: this endpoint is separate from the Search API and has
// much higher limits, so fetches run concurrently via the worker pool.
func (f *Finder) fetchRaw(rawURL string) (string, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	req.Header.Set("User-Agent", "go-github-email-finder")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, rawURL)
	}
	return string(body), nil
}

// searchCommits searches GitHub commits for co-author trailers at the target
// domain, e.g. "Co-authored-by: Name <user@domain>".
func (f *Finder) searchCommits() {
	fmt.Printf("\n[*] Searching GitHub COMMITS for Co-authored-by @%s ...\n", f.domain)

	// Single combined query: both phrases must appear in the commit message.
	query := fmt.Sprintf("\"Co-authored-by\" \"@%s\"", f.domain)
	fmt.Printf("[*] Query: %s\n", query)

	perPage := 100
	for page := 1; page <= 10; page++ {
		params := url.Values{}
		params.Set("q", query)
		params.Set("per_page", fmt.Sprintf("%d", perPage))
		params.Set("page", fmt.Sprintf("%d", page))

		endpoint := commitsSearchURL + "?" + params.Encode()
		body, status, err := f.doRequest(endpoint, "application/vnd.github+json")
		if err != nil {
			fmt.Printf("[!] commits search error: %v\n", err)
			return
		}

		if status != 200 {
			fmt.Printf("[!] commits search HTTP %d: %s\n", status, truncate(string(body), 300))
			return
		}

		var resp CommitsSearchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			fmt.Printf("[!] commits search parse error: %v\n", err)
			return
		}

		if page == 1 {
			fmt.Printf("[*] Commits total matches reported: %d\n", resp.TotalCount)
		}

		if len(resp.Items) == 0 {
			break
		}

		// Extract emails from the commit message (where Co-authored-by trailers live).
		for _, item := range resp.Items {
			src := item.HTMLURL
			for _, m := range f.emailRegex.FindAllString(item.Commit.Message, -1) {
				f.addEmail(m, src)
			}
		}

		if len(resp.Items) < perPage {
			break
		}
	}
}

// printResults prints a sorted summary
func (f *Finder) printResults() {
	fmt.Println("\n==================== RESULTS ====================")
	fmt.Printf("Domain: %s\n", f.domain)
	fmt.Printf("Unique emails found: %d\n\n", len(f.emails))

	if len(f.emails) == 0 {
		fmt.Println("No emails found.")
		return
	}

	keys := make([]string, 0, len(f.emails))
	for k := range f.emails {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		info := f.emails[k]
		fmt.Printf("%s\n", info.Email)
		srcs := make([]string, 0, len(info.Sources))
		for s := range info.Sources {
			srcs = append(srcs, s)
		}
		sort.Strings(srcs)
		// Show up to 3 source references per email to keep output readable
		limit := 3
		if len(srcs) < limit {
			limit = len(srcs)
		}
		for i := 0; i < limit; i++ {
			fmt.Printf("   - %s\n", srcs[i])
		}
		if len(srcs) > limit {
			fmt.Printf("   - ... (+%d more)\n", len(srcs)-limit)
		}
		fmt.Println()
	}
}

// saveResults writes the emails to a text file (one per line)
func (f *Finder) saveResults(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	keys := make([]string, 0, len(f.emails))
	for k := range f.emails {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if _, err := file.WriteString(k + "\n"); err != nil {
			return err
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func main() {
	// Hardcoded default token. Replace the value below with your token,
	// or override at runtime with -t <token> or the GITHUB_TOKEN env var.
	const defaultToken = "" // e.g. "ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"

	domain := flag.String("d", "", "Target domain, e.g. -d hackerone.com")
	tokenFlag := flag.String("t", "", "GitHub personal access token (overrides GITHUB_TOKEN env var and hardcoded default)")
	output := flag.String("o", "", "Optional output file to save emails (one per line)")
	flag.Parse()

	if *domain == "" {
		fmt.Println("Usage: github_email_finder -d <domain> [-t <token>] [-o output.txt]")
		fmt.Println("Example: github_email_finder -d hackerone.com -t ghp_xxx")
		fmt.Println("\nToken precedence: -t flag > GITHUB_TOKEN env var > hardcoded default in source.")
		os.Exit(1)
	}

	// Token precedence: flag > env var > hardcoded default
	token := *tokenFlag
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		token = defaultToken
	}

	if token == "" {
		fmt.Println("[!] Warning: no GitHub token provided.")
		fmt.Println("    - Code search may work but is heavily rate-limited.")
		fmt.Println("    - Commits search REQUIRES authentication and will likely fail.")
		fmt.Println("    Pass one with -t ghp_xxx, or set GITHUB_TOKEN, or hardcode defaultToken.")
		fmt.Println()
	} else {
		// Show only a masked preview so the token isn't echoed in full
		masked := token
		if len(masked) > 8 {
			masked = masked[:4] + "..." + masked[len(masked)-4:]
		}
		fmt.Printf("[*] Using GitHub token: %s\n", masked)
	}

	finder := NewFinder(*domain, token)

	start := time.Now()
	finder.searchCode()
	finder.searchCommits()
	finder.printResults()

	if *output != "" {
		if err := finder.saveResults(*output); err != nil {
			fmt.Printf("[!] Failed to save results: %v\n", err)
		} else {
			fmt.Printf("[*] Emails saved to %s\n", *output)
		}
	}

	fmt.Printf("[*] Done in %s\n", time.Since(start).Round(time.Second))
}
