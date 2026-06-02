package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/gobwas/glob"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

const (
	defaultConfigDir = "configs"
	appDirName       = ".aws-sso-sync"
	cacheFileName    = "cache.json"
	logsFileName     = "logs.jsonl"
)

type SyncProfile struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	RoleName    string `json:"roleName"`
	Alias       string `json:"alias,omitempty"`
	StartURL    string `json:"startUrl,omitempty"`
	Region      string `json:"region,omitempty"`
}

type Snapshot struct {
	FetchedAt time.Time     `json:"fetchedAt"`
	Profiles  []SyncProfile `json:"profiles"`
}

type Override struct {
	MatchRole           string            `json:"matchRole,omitempty"`
	MatchAccount        string            `json:"matchAccount,omitempty"`
	MatchAlias          string            `json:"matchAlias,omitempty"`
	Property            *NameProperty     `json:"property,omitempty"`
	Properties          map[string]string `json:"properties,omitempty"`
	ProfileNameTemplate string            `json:"profileNameTemplate,omitempty"`
	TargetFile          string            `json:"targetFile,omitempty"`
	StartURL            string            `json:"startUrl,omitempty"`
	Region              string            `json:"region,omitempty"`
}

type NameProperty struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type RulesConfig struct {
	Name                string            `json:"name"`
	Enabled             bool              `json:"enabled"`
	TargetFile          string            `json:"targetFile"`
	NameParts           []string          `json:"nameParts,omitempty"`
	Separator           string            `json:"separator,omitempty"`
	Properties          map[string]string `json:"properties,omitempty"`
	ProfileNameTemplate string            `json:"profileNameTemplate"`
	IncludeAccounts     []string          `json:"includeAccounts,omitempty"`
	ExcludeAccounts     []string          `json:"excludeAccounts,omitempty"`
	IncludeRoles        []string          `json:"includeRoles,omitempty"`
	ExcludeRoles        []string          `json:"excludeRoles,omitempty"`
	StartURL            string            `json:"startUrl,omitempty"`
	Region              string            `json:"region,omitempty"`
	Overrides           []Override        `json:"overrides,omitempty"`
}

type LogEvent struct {
	Time           time.Time `json:"time"`
	ConfigName     string    `json:"configName"`
	Action         string    `json:"action"`
	ProfileName    string    `json:"profileName"`
	AccountName    string    `json:"accountName"`
	RoleName       string    `json:"roleName"`
	ManagedProfile bool      `json:"managedProfile"`
}

var funcMap = template.FuncMap{
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"replace": func(old, new, s string) string {
		return strings.ReplaceAll(s, old, new)
	},
	"sanitize": sanitizeName,
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	appDir, err := appDataDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return fmt.Errorf("create app data dir: %w", err)
	}

	cmd := args[0]
	rest := args[1:]

	switch cmd {
	case "format":
		return cmdFormat(appDir)
	case "sync":
		source, err := parseSourceArg(rest)
		if err != nil {
			return err
		}
		return cmdSync(appDir, source)
	case "diff":
		source, err := parseSourceArg(rest)
		if err != nil {
			return err
		}
		return cmdDiff(appDir, source)
	case "list":
		return cmdList(defaultConfigDir)
	case "logs":
		return cmdLogs(appDir)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func parseSourceArg(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) == 2 && args[0] == "--source" {
		if strings.TrimSpace(args[1]) == "" {
			return "", errors.New("--source value is empty")
		}
		return args[1], nil
	}
	return "", errors.New("invalid arguments: expected no args or --source <file>")
}

func cmdSync(appDir string, sourcePath string) error {
	var (
		snapshot Snapshot
		err      error
	)

	if strings.TrimSpace(sourcePath) == "" {
		if err := ensureEndpointSessionsLoggedIn(defaultConfigDir); err != nil {
			return err
		}
		snapshot, err = buildSnapshotFromAWS(defaultConfigDir)
		if err != nil {
			return err
		}
	} else {
		snapshot, err = loadSnapshotFromFile(sourcePath)
		if err != nil {
			return err
		}
	}

	if err := writeSnapshot(filepath.Join(appDir, cacheFileName), snapshot); err != nil {
		return err
	}

	if err := applyConfigs(snapshot, true); err != nil {
		return err
	}

	fmt.Printf("sync complete: cached %d profiles\n", len(snapshot.Profiles))
	return nil
}

func cmdFormat(appDir string) error {
	snapshot, err := loadSnapshotFromFile(filepath.Join(appDir, cacheFileName))
	if err != nil {
		return fmt.Errorf("load cached snapshot: %w", err)
	}

	if err := applyConfigs(snapshot, false); err != nil {
		return err
	}

	fmt.Printf("format complete: rendered %d profiles from cache\n", len(snapshot.Profiles))
	return nil
}

func cmdDiff(appDir string, sourcePath string) error {
	if strings.TrimSpace(sourcePath) == "" {
		return errors.New("diff requires --source <file>")
	}

	oldSnapshot, err := loadSnapshotFromFile(filepath.Join(appDir, cacheFileName))
	if err != nil {
		return fmt.Errorf("load cached snapshot: %w", err)
	}

	newSnapshot, err := loadSnapshotFromFile(sourcePath)
	if err != nil {
		return err
	}

	oldSet := make(map[string]SyncProfile, len(oldSnapshot.Profiles))
	newSet := make(map[string]SyncProfile, len(newSnapshot.Profiles))

	for _, p := range oldSnapshot.Profiles {
		oldSet[profileKey(p)] = p
	}
	for _, p := range newSnapshot.Profiles {
		newSet[profileKey(p)] = p
	}

	added := make([]SyncProfile, 0)
	removed := make([]SyncProfile, 0)

	for key, p := range newSet {
		if _, ok := oldSet[key]; !ok {
			added = append(added, p)
		}
	}
	for key, p := range oldSet {
		if _, ok := newSet[key]; !ok {
			removed = append(removed, p)
		}
	}

	sort.Slice(added, func(i, j int) bool { return profileKey(added[i]) < profileKey(added[j]) })
	sort.Slice(removed, func(i, j int) bool { return profileKey(removed[i]) < profileKey(removed[j]) })

	fmt.Println("added:")
	if len(added) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, p := range added {
			fmt.Printf("  + %s | %s | %s\n", p.AccountName, p.RoleName, p.AccountID)
		}
	}

	fmt.Println("removed:")
	if len(removed) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, p := range removed {
			fmt.Printf("  - %s | %s | %s\n", p.AccountName, p.RoleName, p.AccountID)
		}
	}

	return nil
}

func cmdList(configDir string) error {
	files, err := configFiles(configDir)
	if err != nil {
		return err
	}

	if len(files) == 0 {
		fmt.Println("no configurations found")
		return nil
	}

	for _, file := range files {
		cfg, err := loadRulesConfig(file)
		if err != nil {
			fmt.Printf("%s - invalid (%v)\n", file, err)
			continue
		}
		state := "enabled"
		if !cfg.Enabled {
			state = "disabled"
		}
		fmt.Printf("%s - %s\n", cfg.Name, state)
	}

	return nil
}

func cmdLogs(appDir string) error {
	logPath := filepath.Join(appDir, logsFileName)
	file, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("no logs yet")
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	type lineEvent struct {
		Line  int
		Event LogEvent
	}
	recent := make([]lineEvent, 0, 200)
	line := 0
	for scanner.Scan() {
		line++
		var event LogEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		recent = append(recent, lineEvent{Line: line, Event: event})
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	start := 0
	if len(recent) > 50 {
		start = len(recent) - 50
	}

	for _, rec := range recent[start:] {
		e := rec.Event
		fmt.Printf("%s | %s | %s | %s (%s)\n", e.Time.Format(time.RFC3339), e.ConfigName, e.Action, e.ProfileName, e.AccountName)
	}

	if len(recent) == 0 {
		fmt.Println("no logs yet")
	}

	return nil
}

func applyConfigs(snapshot Snapshot, ensureLogin bool) error {
	files, err := configFiles(defaultConfigDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no configuration files found in %s", defaultConfigDir)
	}

	appDir, err := appDataDir()
	if err != nil {
		return err
	}
	logsPath := filepath.Join(appDir, logsFileName)

	for _, file := range files {
		cfg, err := loadRulesConfig(file)
		if err != nil {
			return fmt.Errorf("load config %s: %w", file, err)
		}
		if !cfg.Enabled {
			continue
		}

		profiles, err := renderProfiles(cfg, snapshot.Profiles)
		if err != nil {
			return fmt.Errorf("render profiles for %s: %w", cfg.Name, err)
		}

		if err := writeManagedProfiles(cfg, profiles, logsPath, ensureLogin); err != nil {
			return fmt.Errorf("write managed block for %s: %w", cfg.Name, err)
		}
	}

	return nil
}

type renderedProfile struct {
	ProfileName string
	AccountName string
	RoleName    string
	SessionName string
	StartURL    string
	Region      string
	Body        string
}

type renderedSession struct {
	Name     string
	StartURL string
	Region   string
}

type profileMetadata struct {
	AccountName string
	RoleName    string
}

func renderProfiles(cfg RulesConfig, raw []SyncProfile) ([]renderedProfile, error) {
	out := make([]renderedProfile, 0, len(raw))
	sessionNames := make(map[string]string)
	for _, p := range raw {
		if !allowedByRules(cfg, p) {
			continue
		}

		effectiveTemplate := cfg.ProfileNameTemplate
		effectiveVars := map[string]string{
			"AccountID":   p.AccountID,
			"AccountName": p.AccountName,
			"RoleName":    p.RoleName,
			"Alias":       p.Alias,
			"StartURL":    p.StartURL,
			"Region":      fallback(p.Region, cfg.Region),
		}
		for key, value := range cfg.Properties {
			cleanKey := strings.TrimSpace(key)
			if cleanKey == "" {
				continue
			}
			resolvedValue, err := renderTemplateString(value, effectiveVars)
			if err != nil {
				return nil, fmt.Errorf("render config property %q: %w", cleanKey, err)
			}
			effectiveVars[cleanKey] = strings.TrimSpace(resolvedValue)
		}
		effectiveTargetStartURL := fallback(p.StartURL, cfg.StartURL)
		effectiveTargetRegion := fallback(p.Region, cfg.Region)

		for _, ov := range cfg.Overrides {
			if !matchesOverride(ov, p) {
				continue
			}
			if ov.ProfileNameTemplate != "" {
				effectiveTemplate = ov.ProfileNameTemplate
			}
			if ov.Property != nil {
				key := strings.TrimSpace(ov.Property.Key)
				if key != "" {
					resolvedValue, err := renderTemplateString(ov.Property.Value, effectiveVars)
					if err != nil {
						return nil, fmt.Errorf("render override property %q: %w", key, err)
					}
					effectiveVars[key] = strings.TrimSpace(resolvedValue)
				}
			}
			for key, value := range ov.Properties {
				cleanKey := strings.TrimSpace(key)
				if cleanKey == "" {
					continue
				}
				resolvedValue, err := renderTemplateString(value, effectiveVars)
				if err != nil {
					return nil, fmt.Errorf("render override property %q: %w", cleanKey, err)
				}
				effectiveVars[cleanKey] = strings.TrimSpace(resolvedValue)
			}
			if ov.StartURL != "" {
				effectiveTargetStartURL = ov.StartURL
			}
			if ov.Region != "" {
				effectiveTargetRegion = ov.Region
			}
		}

		sessionKey := strings.TrimSpace(effectiveTargetStartURL) + "|" + strings.TrimSpace(effectiveTargetRegion)
		sessionName, ok := sessionNames[sessionKey]
		if !ok {
			sessionName = buildSessionName(cfg.Name, effectiveTargetStartURL, effectiveTargetRegion)
			sessionNames[sessionKey] = sessionName
		}

		name, err := renderProfileName(cfg, effectiveTemplate, effectiveVars)
		if err != nil {
			return nil, err
		}

		block := strings.TrimSpace(fmt.Sprintf(
			"[profile %s]\nsso_session = %s\nsso_account_id = %s\nsso_role_name = %s",
			name,
			sessionName,
			p.AccountID,
			p.RoleName,
		))

		out = append(out, renderedProfile{
			ProfileName: name,
			AccountName: p.AccountName,
			RoleName:    p.RoleName,
			SessionName: sessionName,
			StartURL:    effectiveTargetStartURL,
			Region:      effectiveTargetRegion,
			Body:        block,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ProfileName < out[j].ProfileName })
	return out, nil
}

func renderProfileName(cfg RulesConfig, fallbackTemplate string, data map[string]string) (string, error) {
	if len(cfg.NameParts) > 0 {
		separator := cfg.Separator
		parts := make([]string, 0, len(cfg.NameParts))
		for _, rawPart := range cfg.NameParts {
			rendered, err := renderTemplateString(rawPart, data)
			if err != nil {
				return "", err
			}
			rendered = strings.TrimSpace(rendered)
			if rendered == "" {
				continue
			}
			parts = append(parts, rendered)
		}
		return strings.Join(parts, separator), nil
	}

	return applyNameTemplate(fallbackTemplate, data)
}

func writeManagedProfiles(cfg RulesConfig, profiles []renderedProfile, logsPath string, ensureLogin bool) error {
	targetPath, err := expandHome(cfg.TargetFile)
	if err != nil {
		return err
	}
	if absPath, absErr := filepath.Abs(targetPath); absErr == nil {
		targetPath = absPath
	}

	current, err := os.ReadFile(targetPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	marker := cfg.Name
	if marker == "" {
		marker = cfg.Name
	}
	startToken := fmt.Sprintf("# BEGIN AWS-SSO-SYNC %s", marker)
	endToken := fmt.Sprintf("# END AWS-SSO-SYNC %s", marker)

	newBlock := buildManagedBlock(startToken, endToken, profiles)
	updated, oldMeta, _ := replaceManagedBlock(string(current), startToken, endToken, newBlock)
	newMeta := metadataFromRendered(profiles)

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(targetPath, []byte(updated), 0o644); err != nil {
		return err
	}

	if ensureLogin {
		sessions := collectSessions(profiles)
		if err := ensureSSOSessionLogin(targetPath, sessions); err != nil {
			return err
		}
	}

	oldNames := keysFromMetadataMap(oldMeta)
	newNames := keysFromMetadataMap(newMeta)
	removed := difference(oldNames, newNames)
	added := difference(newNames, oldNames)
	if err := appendLogEvents(logsPath, cfg.Name, "added", newMeta, added); err != nil {
		return err
	}
	if err := appendLogEvents(logsPath, cfg.Name, "removed", oldMeta, removed); err != nil {
		return err
	}

	return nil
}

func ensureSSOSessionLogin(configPath string, sessions []renderedSession) error {
	if len(sessions) == 0 {
		return nil
	}
	if _, err := exec.LookPath("aws"); err != nil {
		return errors.New("aws cli not found in PATH")
	}

	for _, session := range sessions {
		if _, err := loadCachedSSOToken(session.StartURL, session.Region); err == nil {
			continue
		}

		fmt.Printf("sso session %s not logged in; running aws sso login\n", session.Name)
		output, err := runSSOLogin(configPath, session.Name, false)
		if err != nil {
			if shouldRetryWithDeviceCode(output) {
				fmt.Printf("browser authorization expired for session %s; retrying with device code\n", session.Name)
				if _, retryErr := runSSOLogin(configPath, session.Name, true); retryErr != nil {
					return fmt.Errorf("aws sso login failed for session %s: %w", session.Name, retryErr)
				}
				continue
			}
			return fmt.Errorf("aws sso login failed for session %s: %w", session.Name, err)
		}
	}

	return nil
}

func runSSOLogin(configPath, sessionName string, useDeviceCode bool) (string, error) {
	args := []string{"sso", "login", "--sso-session", sessionName}
	if useDeviceCode {
		args = append(args, "--use-device-code")
	}
	cmd := exec.Command("aws", args...)
	cmd.Env = append(os.Environ(), "AWS_PAGER=", "AWS_CONFIG_FILE="+configPath)
	cmd.Stdin = os.Stdin

	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	err := cmd.Run()
	output := combined.String()
	if strings.TrimSpace(output) != "" {
		fmt.Print(output)
	}
	return output, err
}

func shouldRetryWithDeviceCode(output string) bool {
	text := strings.ToLower(output)
	return strings.Contains(text, "pending authorization") && strings.Contains(text, "expired")
}

func ensureEndpointSessionsLoggedIn(configDir string) error {
	files, err := configFiles(configDir)
	if err != nil {
		return err
	}

	seen := make(map[string]struct{})
	sessions := make([]renderedSession, 0)
	for _, file := range files {
		cfg, err := loadRulesConfig(file)
		if err != nil {
			return fmt.Errorf("load config %s: %w", file, err)
		}
		if !cfg.Enabled {
			continue
		}
		if strings.TrimSpace(cfg.StartURL) == "" || strings.TrimSpace(cfg.Region) == "" {
			continue
		}

		key := strings.TrimSpace(cfg.StartURL) + "|" + strings.TrimSpace(cfg.Region)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		sessions = append(sessions, renderedSession{
			Name:     buildSessionName(cfg.Name, cfg.StartURL, cfg.Region),
			StartURL: cfg.StartURL,
			Region:   cfg.Region,
		})
	}

	missing := make([]renderedSession, 0)
	for _, session := range sessions {
		if _, err := loadCachedSSOToken(session.StartURL, session.Region); err == nil {
			continue
		}
		missing = append(missing, session)
	}
	if len(missing) == 0 {
		return nil
	}

	tempConfig, cleanup, err := writeTempSSOConfig(missing)
	if err != nil {
		return err
	}
	defer cleanup()

	return ensureSSOSessionLogin(tempConfig, missing)
}

func writeTempSSOConfig(sessions []renderedSession) (string, func(), error) {
	f, err := os.CreateTemp("", "aws-sso-sync-config-*.ini")
	if err != nil {
		return "", nil, err
	}

	b := strings.Builder{}
	for _, session := range sessions {
		b.WriteString(fmt.Sprintf("[sso-session %s]\n", session.Name))
		b.WriteString(fmt.Sprintf("sso_start_url = %s\n", session.StartURL))
		b.WriteString(fmt.Sprintf("sso_region = %s\n", session.Region))
		b.WriteString("sso_registration_scopes = sso:account:access\n\n")
	}

	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", nil, err
	}

	cleanup := func() {
		_ = os.Remove(f.Name())
	}
	return f.Name(), cleanup, nil
}

func buildManagedBlock(startToken, endToken string, profiles []renderedProfile) string {
	b := strings.Builder{}
	b.WriteString(startToken)
	b.WriteString("\n")

	sessions := collectSessions(profiles)
	for _, session := range sessions {
		b.WriteString(fmt.Sprintf("[sso-session %s]\nsso_start_url = %s\nsso_region = %s\nsso_registration_scopes = sso:account:access\n\n", session.Name, session.StartURL, session.Region))
	}

	for i, p := range profiles {
		b.WriteString(p.Body)
		if i < len(profiles)-1 {
			b.WriteString("\n\n")
		} else {
			b.WriteString("\n")
		}
	}
	b.WriteString(endToken)
	b.WriteString("\n")
	return b.String()
}

func collectSessions(profiles []renderedProfile) []renderedSession {
	byName := make(map[string]renderedSession)
	for _, p := range profiles {
		if strings.TrimSpace(p.SessionName) == "" {
			continue
		}
		byName[p.SessionName] = renderedSession{
			Name:     p.SessionName,
			StartURL: p.StartURL,
			Region:   p.Region,
		}
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]renderedSession, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func buildSessionName(configName, startURL, region string) string {
	base := configName
	if base == "" {
		base = "aws-sso-sync"
	}
	regionPart := region
	if regionPart == "" {
		regionPart = "region"
	}
	hostPart := "sso"
	if parsed, err := url.Parse(strings.TrimSpace(startURL)); err == nil {
		host := strings.TrimSpace(parsed.Hostname())
		host = strings.TrimPrefix(host, "www.")
		if host != "" {
			hostPart = host
		}
	}
	return sanitizeName(fmt.Sprintf("%s-%s-%s", base, regionPart, hostPart))
}

func replaceManagedBlock(content, startToken, endToken, replacement string) (string, map[string]profileMetadata, map[string]profileMetadata) {
	start := strings.Index(content, startToken)
	if start == -1 {
		trimmed := strings.TrimSpace(content)
		newMeta := extractProfileMetadata(replacement)
		if trimmed == "" {
			return replacement, map[string]profileMetadata{}, newMeta
		}
		return trimmed + "\n\n" + replacement, map[string]profileMetadata{}, newMeta
	}

	end := strings.Index(content[start:], endToken)
	if end == -1 {
		end = len(content)
	} else {
		end = start + end + len(endToken)
		if end < len(content) && content[end] == '\n' {
			end++
		}
	}

	oldBlock := content[start:end]
	oldMeta := extractProfileMetadata(oldBlock)
	newMeta := extractProfileMetadata(replacement)
	updated := content[:start] + replacement + content[end:]
	return updated, oldMeta, newMeta
}

func metadataFromRendered(profiles []renderedProfile) map[string]profileMetadata {
	out := make(map[string]profileMetadata, len(profiles))
	for _, p := range profiles {
		out[p.ProfileName] = profileMetadata{
			AccountName: p.AccountName,
			RoleName:    p.RoleName,
		}
	}
	return out
}

func extractProfileMetadata(text string) map[string]profileMetadata {
	result := make(map[string]profileMetadata)
	var current string
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "[profile ") && strings.HasSuffix(line, "]") {
			current = strings.TrimSuffix(strings.TrimPrefix(line, "[profile "), "]")
			if _, ok := result[current]; !ok {
				result[current] = profileMetadata{}
			}
			continue
		}
		if current == "" {
			continue
		}
		if strings.HasPrefix(line, "sso_role_name =") {
			meta := result[current]
			meta.RoleName = strings.TrimSpace(strings.TrimPrefix(line, "sso_role_name ="))
			result[current] = meta
		}
	}
	return result
}

func keysFromMetadataMap(m map[string]profileMetadata) []string {
	keys := make([]string, 0, len(m))
	for name := range m {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func appendLogEvents(logPath, configName, action string, byName map[string]profileMetadata, profileNames []string) error {
	if len(profileNames) == 0 {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	sort.Strings(profileNames)
	enc := json.NewEncoder(f)
	for _, name := range profileNames {
		meta := byName[name]
		event := LogEvent{
			Time:           time.Now().UTC(),
			ConfigName:     configName,
			Action:         action,
			ProfileName:    name,
			AccountName:    meta.AccountName,
			RoleName:       meta.RoleName,
			ManagedProfile: true,
		}
		if err := enc.Encode(event); err != nil {
			return err
		}
	}

	return nil
}

func allowedByRules(cfg RulesConfig, p SyncProfile) bool {
	if len(cfg.IncludeAccounts) > 0 && !contains(cfg.IncludeAccounts, p.AccountName) && !contains(cfg.IncludeAccounts, p.AccountID) {
		return false
	}
	if len(cfg.IncludeRoles) > 0 && !contains(cfg.IncludeRoles, p.RoleName) {
		return false
	}
	if contains(cfg.ExcludeAccounts, p.AccountName) || contains(cfg.ExcludeAccounts, p.AccountID) {
		return false
	}
	if contains(cfg.ExcludeRoles, p.RoleName) {
		return false
	}
	return true
}

func contains(values []string, candidate string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func difference(a, b []string) []string {
	bSet := make(map[string]struct{}, len(b))
	for _, v := range b {
		bSet[v] = struct{}{}
	}
	out := make([]string, 0)
	for _, v := range a {
		if _, ok := bSet[v]; !ok {
			out = append(out, v)
		}
	}
	return out
}

func matchesOverride(ov Override, p SyncProfile) bool {
	if ov.MatchRole != "" {
		var g = glob.MustCompile(ov.MatchRole)
		if !g.Match(p.RoleName) {
			return false
		}
	}
	if ov.MatchAccount != "" {
		var g = glob.MustCompile(ov.MatchAccount)
		if !g.Match(p.AccountName) {
			return false
		}
	}
	if ov.MatchAlias != "" {
		var g = glob.MustCompile(ov.MatchAlias)
		if !g.Match(p.Alias) {
			return false
		}
	}
	return true
}

func applyNameTemplate(tmpl string, data map[string]string) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = "{{.Prefix}}{{.AccountName}}-{{.RoleName}}"
	}
	rendered, err := renderTemplateString(tmpl, data)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(rendered), nil
}

func renderTemplateString(tmpl string, data map[string]string) (string, error) {
	t, err := template.New("name").Funcs(funcMap).Option("missingkey=zero").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer(" ", "-", "/", "-", "_", "-", "\\", "-", ":", "-")
	s = replacer.Replace(s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

func loadRulesConfig(path string) (RulesConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RulesConfig{}, err
	}
	var cfg RulesConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return RulesConfig{}, err
	}
	if cfg.Name == "" {
		cfg.Name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	if cfg.TargetFile == "" {
		cfg.TargetFile = "~/.aws/config"
	}
	if cfg.ProfileNameTemplate == "" {
		cfg.ProfileNameTemplate = "{{.Prefix}}{{.AccountName}}-{{.RoleName}}"
	}
	if cfg.Separator == "" {
		cfg.Separator = "-"
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return cfg, nil
}

func configFiles(configDir string) ([]string, error) {
	entries := make([]string, 0)
	err := filepath.WalkDir(configDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".json" {
			entries = append(entries, path)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(entries)
	return entries, nil
}

func loadSnapshotFromFile(path string) (Snapshot, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, err
	}
	if snap.FetchedAt.IsZero() {
		snap.FetchedAt = time.Now().UTC()
	}
	return snap, nil
}

func writeSnapshot(path string, snap Snapshot) error {
	if snap.FetchedAt.IsZero() {
		snap.FetchedAt = time.Now().UTC()
	}
	raw, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func profileKey(p SyncProfile) string {
	return p.AccountID + "|" + p.AccountName + "|" + p.RoleName
}

func appDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, appDirName), nil
}

func expandHome(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

func fallback(primary, secondary string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return secondary
}

func printUsage() {
	fmt.Println("aws-sso-sync usage:")
	fmt.Println("  aws-sso-sync format")
	fmt.Println("  aws-sso-sync sync")
	fmt.Println("  aws-sso-sync sync --source <snapshot.json>")
	fmt.Println("  aws-sso-sync diff --source <snapshot.json>")
	fmt.Println("  aws-sso-sync list")
	fmt.Println("  aws-sso-sync logs")
	fmt.Println()
	fmt.Println("snapshot schema:")
	fmt.Println(`  {
    "fetchedAt": "2026-06-01T00:00:00Z",
    "profiles": [
      {
        "accountId": "123456789012",
        "accountName": "Production",
        "roleName": "AdministratorAccess",
        "startUrl": "https://example.awsapps.com/start",
        "region": "us-east-1"
      }
    ]
  }`)
}

type ssoEndpoint struct {
	StartURL string
	Region   string
}

type cachedSSOToken struct {
	StartURL    string `json:"startUrl"`
	Region      string `json:"region"`
	AccessToken string `json:"accessToken"`
	ExpiresAt   string `json:"expiresAt"`
}

type awsListAccountsResponse struct {
	AccountList []struct {
		AccountID   string `json:"accountId"`
		AccountName string `json:"accountName"`
		Email       string `json:"emailAddress"`
	} `json:"accountList"`
	NextToken string `json:"nextToken"`
}

type awsListAccountRolesResponse struct {
	RoleList []struct {
		RoleName string `json:"roleName"`
	} `json:"roleList"`
	NextToken string `json:"nextToken"`
}

func buildSnapshotFromAWS(configDir string) (Snapshot, error) {
	if _, err := exec.LookPath("aws"); err != nil {
		return Snapshot{}, errors.New("aws cli not found in PATH")
	}

	endpoints, err := discoverEnabledEndpoints(configDir)
	if err != nil {
		return Snapshot{}, err
	}
	if len(endpoints) == 0 {
		return Snapshot{}, fmt.Errorf("no enabled configs with startUrl/region found in %s", configDir)
	}

	all := make([]SyncProfile, 0)
	seen := make(map[string]struct{})

	for _, endpoint := range endpoints {
		token, err := loadCachedSSOToken(endpoint.StartURL, endpoint.Region)
		if err != nil {
			return Snapshot{}, fmt.Errorf("load sso token for %s (%s): %w", endpoint.StartURL, endpoint.Region, err)
		}

		profiles, err := fetchProfilesFromAWS(token, endpoint.StartURL, endpoint.Region)
		if err != nil {
			return Snapshot{}, err
		}

		for _, p := range profiles {
			key := profileKey(p) + "|" + p.StartURL + "|" + p.Region
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			all = append(all, p)
		}
	}

	sort.Slice(all, func(i, j int) bool {
		left := all[i].AccountName + "|" + all[i].RoleName + "|" + all[i].AccountID
		right := all[j].AccountName + "|" + all[j].RoleName + "|" + all[j].AccountID
		return left < right
	})

	return Snapshot{
		FetchedAt: time.Now().UTC(),
		Profiles:  all,
	}, nil
}

func discoverEnabledEndpoints(configDir string) ([]ssoEndpoint, error) {
	files, err := configFiles(configDir)
	if err != nil {
		return nil, err
	}

	endpoints := make([]ssoEndpoint, 0)
	seen := make(map[string]struct{})

	for _, path := range files {
		cfg, err := loadRulesConfig(path)
		if err != nil {
			return nil, err
		}
		if !cfg.Enabled {
			continue
		}
		if strings.TrimSpace(cfg.StartURL) == "" || strings.TrimSpace(cfg.Region) == "" {
			continue
		}
		key := cfg.StartURL + "|" + cfg.Region
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		endpoints = append(endpoints, ssoEndpoint{StartURL: cfg.StartURL, Region: cfg.Region})
	}

	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].StartURL+"|"+endpoints[i].Region < endpoints[j].StartURL+"|"+endpoints[j].Region
	})

	return endpoints, nil
}

func loadCachedSSOToken(startURL, region string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	cacheGlob := filepath.Join(home, ".aws", "sso", "cache", "*.json")
	matches, err := filepath.Glob(cacheGlob)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", errors.New("no cached SSO tokens found; run aws sso login first")
	}

	now := time.Now().UTC()
	bestExpiry := time.Time{}
	bestToken := ""
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var token cachedSSOToken
		if err := json.Unmarshal(raw, &token); err != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(token.StartURL), strings.TrimSpace(startURL)) {
			continue
		}
		if strings.TrimSpace(token.Region) != "" && !strings.EqualFold(strings.TrimSpace(token.Region), strings.TrimSpace(region)) {
			continue
		}
		expiresAt, err := parseTokenExpiry(token.ExpiresAt)
		if err != nil {
			continue
		}
		if expiresAt.Before(now) {
			continue
		}
		if strings.TrimSpace(token.AccessToken) == "" {
			continue
		}
		if expiresAt.After(bestExpiry) {
			bestExpiry = expiresAt
			bestToken = token.AccessToken
		}
	}

	if bestToken == "" {
		return "", fmt.Errorf("no valid SSO token for startUrl=%s region=%s; run aws sso login", startURL, region)
	}

	return bestToken, nil
}

func parseTokenExpiry(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty expiresAt")
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05UTC",
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05.000UTC",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized expiresAt format: %s", value)
}

func fetchProfilesFromAWS(accessToken, startURL, region string) ([]SyncProfile, error) {
	accounts, err := awsListAccounts(accessToken, region)
	if err != nil {
		return nil, err
	}

	profiles := make([]SyncProfile, 0)
	for _, account := range accounts {
		roles, err := awsListAccountRoles(accessToken, region, account.AccountID)
		if err != nil {
			return nil, err
		}
		for _, role := range roles {
			profiles = append(profiles, SyncProfile{
				AccountID:   account.AccountID,
				AccountName: account.AccountName,
				RoleName:    role.RoleName,
				Alias:       account.Email,
				StartURL:    startURL,
				Region:      region,
			})
		}
	}

	return profiles, nil
}

func awsListAccounts(accessToken, region string) ([]struct {
	AccountID   string `json:"accountId"`
	AccountName string `json:"accountName"`
	Email       string `json:"emailAddress"`
}, error) {
	all := make([]struct {
		AccountID   string `json:"accountId"`
		AccountName string `json:"accountName"`
		Email       string `json:"emailAddress"`
	}, 0)
	nextToken := ""

	for {
		args := []string{"sso", "list-accounts", "--access-token", accessToken, "--region", region, "--output", "json"}
		if nextToken != "" {
			args = append(args, "--next-token", nextToken)
		}
		out, err := runAWSCommand(args, "sso list-accounts")
		if err != nil {
			return nil, err
		}

		var response awsListAccountsResponse
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("parse list-accounts response: %w", err)
		}
		all = append(all, response.AccountList...)
		if strings.TrimSpace(response.NextToken) == "" {
			break
		}
		nextToken = response.NextToken
	}

	return all, nil
}

func awsListAccountRoles(accessToken, region, accountID string) ([]struct {
	RoleName string `json:"roleName"`
}, error) {
	all := make([]struct {
		RoleName string `json:"roleName"`
	}, 0)
	nextToken := ""

	for {
		args := []string{"sso", "list-account-roles", "--access-token", accessToken, "--region", region, "--account-id", accountID, "--output", "json"}
		if nextToken != "" {
			args = append(args, "--next-token", nextToken)
		}
		out, err := runAWSCommand(args, "sso list-account-roles")
		if err != nil {
			return nil, fmt.Errorf("list roles for account %s: %w", accountID, err)
		}

		var response awsListAccountRolesResponse
		if err := json.Unmarshal(out, &response); err != nil {
			return nil, fmt.Errorf("parse list-account-roles response: %w", err)
		}
		all = append(all, response.RoleList...)
		if strings.TrimSpace(response.NextToken) == "" {
			break
		}
		nextToken = response.NextToken
	}

	return all, nil
}

func runAWSCommand(args []string, operation string) ([]byte, error) {
	cmd := exec.Command("aws", args...)
	cmd.Env = append(os.Environ(), "AWS_PAGER=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("aws %s failed: %s", operation, strings.TrimSpace(string(out)))
	}
	return out, nil
}
