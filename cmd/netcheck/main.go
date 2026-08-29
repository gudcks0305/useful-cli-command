package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const defaultTimeout = 5 * time.Second

type targetSpec struct {
	raw     string
	display string
	host    string
	port    string
	useTLS  bool
	useHTTP bool
}

type phaseResult struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

type httpResult struct {
	StatusCode int    `json:"status_code"`
	Status     string `json:"status"`
}

type certificateResult struct {
	Subject    string `json:"subject"`
	SANCount   int    `json:"san_count"`
	ExpiresAt  string `json:"expires_at"`
	ExpiryDays int    `json:"expiry_days"`
}

type checkResult struct {
	Target      string             `json:"target"`
	Timeout     string             `json:"timeout"`
	Phases      []phaseResult      `json:"phases"`
	HTTP        *httpResult        `json:"http,omitempty"`
	Certificate *certificateResult `json:"certificate,omitempty"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("netcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "output JSON")
	timeout := flags.Duration("timeout", defaultTimeout, "overall timeout")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: netcheck [--json] [--timeout D] <https://URL|http://URL|host[:port]>")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "netcheck: --timeout must be greater than zero")
		return 2
	}

	spec, err := parseTarget(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "netcheck: %v\n", err)
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, timedOut := checkTarget(ctx, spec, *timeout, nil)
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintf(stderr, "netcheck: write output: %v\n", err)
			return 1
		}
	} else {
		printText(stdout, result)
	}

	return resultExitCode(result, timedOut)
}

func resultExitCode(result checkResult, timedOut bool) int {
	if timedOut {
		return 3
	}
	for _, phase := range result.Phases {
		if phase.Status == "failed" {
			return 1
		}
	}
	return 0
}

func parseTarget(input string) (targetSpec, error) {
	if input == "" || strings.TrimSpace(input) != input || strings.IndexFunc(input, unicode.IsSpace) >= 0 {
		return targetSpec{}, errors.New("target must be one non-empty value without whitespace")
	}
	if strings.Contains(input, "://") {
		return parseURLTarget(input)
	}
	if _, _, err := net.ParseCIDR(input); err == nil {
		return targetSpec{}, errors.New("CIDR targets are not supported")
	}
	if strings.ContainsAny(input, "/?#@%") {
		return targetSpec{}, errors.New("host target contains unsupported characters")
	}

	host, port, err := splitHostPort(input)
	if err != nil {
		return targetSpec{}, err
	}
	if err := validateHost(host); err != nil {
		return targetSpec{}, err
	}
	if port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return targetSpec{}, errors.New("port must be a number from 1 to 65535")
		}
	}
	display := host
	if port != "" {
		display = net.JoinHostPort(host, port)
	}
	return targetSpec{
		raw:     input,
		display: display,
		host:    host,
		port:    port,
		useTLS:  port == "443",
	}, nil
}

func parseURLTarget(input string) (targetSpec, error) {
	u, err := url.ParseRequestURI(input)
	if err != nil {
		return targetSpec{}, fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return targetSpec{}, errors.New("URL scheme must be http or https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return targetSpec{}, errors.New("URL must include a host")
	}
	if u.User != nil {
		return targetSpec{}, errors.New("URL credentials are not supported")
	}
	if err := validateHost(u.Hostname()); err != nil {
		return targetSpec{}, err
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return targetSpec{}, errors.New("URL port must be a number from 1 to 65535")
		}
	}
	displayURL := *u
	displayURL.RawQuery = ""
	displayURL.ForceQuery = false
	displayURL.Fragment = ""
	return targetSpec{
		raw:     input,
		display: displayURL.String(),
		host:    u.Hostname(),
		port:    port,
		useTLS:  u.Scheme == "https",
		useHTTP: true,
	}, nil
}

func splitHostPort(input string) (string, string, error) {
	if strings.HasPrefix(input, "[") && strings.HasSuffix(input, "]") {
		host := strings.TrimSuffix(strings.TrimPrefix(input, "["), "]")
		if net.ParseIP(host) == nil {
			return "", "", errors.New("invalid bracketed IP address")
		}
		return host, "", nil
	}
	if host, port, err := net.SplitHostPort(input); err == nil {
		if host == "" {
			return "", "", errors.New("host must not be empty")
		}
		return host, port, nil
	}
	if strings.Count(input, ":") > 1 {
		if net.ParseIP(input) == nil {
			return "", "", errors.New("invalid IPv6 target; use [address]:port when specifying a port")
		}
		return input, "", nil
	}
	if strings.Contains(input, ":") {
		return "", "", errors.New("invalid host:port target")
	}
	return input, "", nil
}

func validateHost(host string) error {
	if host == "" || len(host) > 253 {
		return errors.New("invalid host")
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	trimmed := strings.TrimSuffix(host, ".")
	if trimmed == "" {
		return errors.New("invalid host")
	}
	for _, label := range strings.Split(trimmed, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("invalid host name")
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
				return errors.New("invalid host name")
			}
		}
	}
	return nil
}

func checkTarget(ctx context.Context, spec targetSpec, timeout time.Duration, roots *x509.CertPool) (checkResult, bool) {
	result := checkResult{Target: spec.display, Timeout: timeout.String()}
	result.Phases = append(result.Phases, phaseResult{Name: "dns", Status: "pending"})
	if spec.port != "" {
		result.Phases = append(result.Phases, phaseResult{Name: "tcp", Status: "pending"})
	}
	if spec.useTLS {
		result.Phases = append(result.Phases, phaseResult{Name: "tls", Status: "pending"})
	}
	if spec.useHTTP {
		result.Phases = append(result.Phases, phaseResult{Name: "http", Status: "pending"})
	}

	dnsIndex := phaseIndex(result.Phases, "dns")
	started := time.Now()
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, spec.host)
	finishPhase(&result.Phases[dnsIndex], started, err)
	if err != nil || len(addresses) == 0 {
		if err == nil {
			err = errors.New("no addresses found")
			result.Phases[dnsIndex].Status = "failed"
			result.Phases[dnsIndex].Error = err.Error()
		}
		skipPending(result.Phases, "dependency failed")
		return result, isTimeout(err, ctx)
	}

	if spec.port == "" {
		return result, false
	}
	tcpIndex := phaseIndex(result.Phases, "tcp")
	started = time.Now()
	conn, err := dialAddresses(ctx, addresses, spec.port)
	finishPhase(&result.Phases[tcpIndex], started, err)
	if err != nil {
		skipPending(result.Phases, "dependency failed")
		return result, isTimeout(err, ctx)
	}

	if spec.useTLS {
		tlsIndex := phaseIndex(result.Phases, "tls")
		started = time.Now()
		tlsConn := tls.Client(conn, tlsConfig(spec.host, roots))
		err = tlsConn.HandshakeContext(ctx)
		finishPhase(&result.Phases[tlsIndex], started, err)
		if err != nil {
			conn.Close()
			skipPending(result.Phases, "dependency failed")
			return result, isTimeout(err, ctx)
		}
		result.Certificate = certificateInfo(tlsConn.ConnectionState(), time.Now())
		conn = tlsConn
	}
	if spec.useHTTP {
		return result, checkHTTP(ctx, spec, conn, &result)
	}
	defer conn.Close()
	return result, false
}

func checkHTTP(ctx context.Context, spec targetSpec, conn net.Conn, result *checkResult) bool {
	httpIndex := phaseIndex(result.Phases, "http")
	transport := &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
	}
	used := false
	provideConnection := func() (net.Conn, error) {
		if used {
			return nil, errors.New("unexpected second connection attempt")
		}
		used = true
		return conn, nil
	}
	if spec.useTLS {
		transport.DialTLSContext = func(context.Context, string, string) (net.Conn, error) {
			return provideConnection()
		}
	} else {
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return provideConnection()
		}
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer transport.CloseIdleConnections()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, spec.raw, nil)
	if err != nil {
		conn.Close()
		finishPhase(&result.Phases[httpIndex], time.Now(), err)
		return false
	}
	started := time.Now()
	response, err := client.Do(request)
	total := time.Since(started)
	if err != nil {
		finishPhase(&result.Phases[httpIndex], started, err)
		return isTimeout(err, ctx)
	}
	defer response.Body.Close()
	result.HTTP = &httpResult{StatusCode: response.StatusCode, Status: response.Status}
	result.Phases[httpIndex].DurationMS = durationMS(total)
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		result.Phases[httpIndex].Status = "ok"
	} else {
		result.Phases[httpIndex].Status = "failed"
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			result.Phases[httpIndex].Error = "redirect not followed"
		} else {
			result.Phases[httpIndex].Error = "HTTP status outside 2xx range"
		}
	}
	return false
}

func tlsConfig(host string, roots *x509.CertPool) *tls.Config {
	return &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12}
}

func dialAddresses(ctx context.Context, addresses []net.IPAddr, port string) (net.Conn, error) {
	var lastErr error
	dialer := net.Dialer{}
	for _, address := range addresses {
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no addresses found")
	}
	return nil, lastErr
}

func finishPhase(phase *phaseResult, started time.Time, err error) {
	phase.DurationMS = durationMS(time.Since(started))
	if err == nil {
		phase.Status = "ok"
		phase.Error = ""
		return
	}
	phase.Status = "failed"
	phase.Error = safeError(err)
}

func durationMS(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func safeError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded.Error()
	}
	return err.Error()
}

func isTimeout(err error, ctx context.Context) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func phaseIndex(phases []phaseResult, name string) int {
	for index := range phases {
		if phases[index].Name == name {
			return index
		}
	}
	return -1
}

func skipPending(phases []phaseResult, reason string) {
	for index := range phases {
		if phases[index].Status == "pending" {
			phases[index].Status = "skipped"
			phases[index].Error = reason
		}
	}
}

func certificateInfo(state tls.ConnectionState, now time.Time) *certificateResult {
	if len(state.PeerCertificates) == 0 {
		return nil
	}
	certificate := state.PeerCertificates[0]
	subject := certificate.Subject.CommonName
	if subject == "" {
		subject = certificate.Subject.String()
	}
	sanCount := len(certificate.DNSNames) + len(certificate.IPAddresses) + len(certificate.EmailAddresses) + len(certificate.URIs)
	expiryDays := int(math.Floor(certificate.NotAfter.Sub(now).Hours() / 24))
	return &certificateResult{
		Subject:    subject,
		SANCount:   sanCount,
		ExpiresAt:  certificate.NotAfter.UTC().Format(time.RFC3339),
		ExpiryDays: expiryDays,
	}
}

func printText(writer io.Writer, result checkResult) {
	fmt.Fprintf(writer, "target: %s\n", result.Target)
	fmt.Fprintf(writer, "timeout: %s\n", result.Timeout)
	for _, phase := range result.Phases {
		fmt.Fprintf(writer, "%s: %s (%dms)", phase.Name, phase.Status, phase.DurationMS)
		if phase.Error != "" {
			fmt.Fprintf(writer, " - %s", phase.Error)
		}
		fmt.Fprintln(writer)
	}
	if result.HTTP != nil {
		fmt.Fprintf(writer, "http_status: %s\n", result.HTTP.Status)
	}
	if result.Certificate != nil {
		fmt.Fprintf(writer, "certificate: subject=%q san_count=%d expires_at=%s expiry_days=%d\n",
			result.Certificate.Subject,
			result.Certificate.SANCount,
			result.Certificate.ExpiresAt,
			result.Certificate.ExpiryDays,
		)
	}
}
