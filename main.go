package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"flag" // Added for CLI flags
	"fmt"
	"html/template"
	"io"
	"io/fs"
	stdlog "log" // Standard log for initial pre-zerolog setup errors
	"net/http"
	"net/url"
	"os"
	"os/exec" // For systemd commands
	"os/signal"
	"path/filepath" // For systemd service file path
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log" // Global zerolog logger
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*
var templatesFS embed.FS

//go:embed static
var staticFilesFS embed.FS

var parsedStatusTemplate *template.Template
var parsedConfigTemplate *template.Template

type HealthStatus string

const (
	StatusUnknown   HealthStatus = "UNKNOWN"
	StatusHealthy   HealthStatus = "HEALTHY"
	StatusUnhealthy HealthStatus = "UNHEALTHY"
	StatusWarning   HealthStatus = "WARNING"
)

type MonitorType string

const (
	TypeDisk        MonitorType = "Disk"
	TypeCPU         MonitorType = "CPU"
	TypeMemory      MonitorType = "Memory"
	TypeApplication MonitorType = "Application"
)

type MonitoredItem struct {
	Name           string
	Type           MonitorType
	Status         HealthStatus
	Details        string
	LastCheckTime  time.Time
	PreviousStatus HealthStatus
	Icon           string
	PathOrURL      string
	Threshold      float64
	AppConfig      *AppMonitorConfig
	DiskConfig     *DiskMonitorConfig
	CPUConfig      *CPUMonitorConfig
	MemoryConfig   *MemoryMonitorConfig
}

type Config struct {
	Server                 ServerConfig
	Webhooks               []WebhookConfig
	Disks                  []DiskMonitorConfig
	CPU                    CPUMonitorConfig
	Memory                 MemoryMonitorConfig
	Applications           []AppMonitorConfig
	MonitorIntervalSeconds int
	Logging                LoggingConfig
}

type LoggingConfig struct {
	Format string
	Level  string
}

type ServerConfig struct {
	Host        string
	Port        int
	IconBaseURL string
}

type WebhookConfig struct {
	Name    string
	URL     string
	Method  string
	Headers map[string]string
}

type DiskMonitorConfig struct {
	Name      string
	Path      string
	Threshold float64
	Icon      string
}

type CPUMonitorConfig struct {
	Enabled   bool
	Threshold float64
	Icon      string
}

type MemoryMonitorConfig struct {
	Enabled   bool
	Threshold float64
	Icon      string
}

type AppMonitorConfig struct {
	Name               string
	URL                string
	ExpectedStatusCode int
	TimeoutSeconds     int
	Threshold          AppThresholdConfig
	Icon               string
	IconFetch          bool
}

type AppThresholdConfig struct {
	MaxResponseTimeMS int
	BodyContains      string
	BodyNotContains   string
}

var Cfg Config
var MonitoredItems []*MonitoredItem
var itemsMutex sync.RWMutex

// --- Service Management Functions ---

const serviceName = "go-monitor.service"
const serviceFilePathBase = "/etc/systemd/system/" // Standard path for system services

func installSystemdService(configDir string) error {
	zlog.Info().Msg("Attempting to install systemd service...")
	if os.Geteuid() != 0 {
		zlog.Error().Msg("Systemd service installation requires root privileges (sudo). Please re-run with sudo.")
		return fmt.Errorf("permission denied: service installation requires sudo")
	}

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not get executable path: %w", err)
	}
	workDir := filepath.Dir(exePath)
	zlog.Info().Str("executable_path", exePath).Str("working_directory", workDir).Msg("Service details")

	// If configDir is relative, make it absolute based on working directory
	absConfigDir := configDir
	if !filepath.IsAbs(configDir) {
		absConfigDir, err = filepath.Abs(filepath.Join(workDir, configDir))
		if err != nil {
			return fmt.Errorf("could not determine absolute path for config directory %s: %w", configDir, err)
		}
	}
	zlog.Info().Str("config_dir_effective", absConfigDir).Msg("Service will use this config directory")


	serviceContent := fmt.Sprintf(`[Unit]
Description=Go Monitor Application
After=network.target

[Service]
Type=simple
ExecStart=%s --config-dir %s
WorkingDirectory=%s
Restart=on-failure
RestartSec=5s
# Ensure the user running this has permissions to read the config-dir
# User=gomonitor # Consider running as a non-root user
# Group=gomonitor

[Install]
WantedBy=multi-user.target
`, exePath, absConfigDir, workDir)

	serviceFilePath := filepath.Join(serviceFilePathBase, serviceName)
	zlog.Info().Str("service_file_path", serviceFilePath).Msg("Writing service file")
	err = os.WriteFile(serviceFilePath, []byte(serviceContent), 0644)
	if err != nil {
		return fmt.Errorf("failed to write systemd service file to %s: %w", serviceFilePath, err)
	}

	commands := [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", serviceName},
		{"systemctl", "start", serviceName},
	}

	for _, cmdArgs := range commands {
		zlog.Info().Strs("command", cmdArgs).Msg("Running systemctl command")
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		output, execErr := cmd.CombinedOutput()
		if execErr != nil {
			return fmt.Errorf("failed to run '%s': %w. Output: %s", strings.Join(cmdArgs, " "), execErr, string(output))
		}
		zlog.Debug().Str("command", strings.Join(cmdArgs, " ")).Bytes("output", output).Msg("Command executed")
	}

	zlog.Info().Msg("Systemd service installed and started successfully.")
	return nil
}

func removeSystemdService() error {
	zlog.Info().Msg("Attempting to remove systemd service...")
	if os.Geteuid() != 0 {
		zlog.Error().Msg("Systemd service removal requires root privileges (sudo). Please re-run with sudo.")
		return fmt.Errorf("permission denied: service removal requires sudo")
	}

	commands := [][]string{
		{"systemctl", "stop", serviceName},
		{"systemctl", "disable", serviceName},
	}
	for _, cmdArgs := range commands {
		zlog.Info().Strs("command", cmdArgs).Msg("Running systemctl command")
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		output, execErr := cmd.CombinedOutput()
		if execErr != nil {
			zlog.Error().Err(execErr).Str("command", strings.Join(cmdArgs, " ")).Bytes("output", output).Msg("Error executing command (continuing)")
		} else {
			zlog.Debug().Str("command", strings.Join(cmdArgs, " ")).Bytes("output", output).Msg("Command executed")
		}
	}

	serviceFilePath := filepath.Join(serviceFilePathBase, serviceName)
	zlog.Info().Str("service_file_path", serviceFilePath).Msg("Removing service file")
	err := os.Remove(serviceFilePath)
	if err != nil && !os.IsNotExist(err) {
		zlog.Error().Err(err).Str("path", serviceFilePath).Msg("Failed to remove service file (continuing, try manual removal)")
	}

	zlog.Info().Msg("Running systemctl daemon-reload")
	cmdDaemonReload := exec.Command("systemctl", "daemon-reload")
	outputDaemonReload, errDaemonReload := cmdDaemonReload.CombinedOutput()
	if errDaemonReload != nil {
		return fmt.Errorf("failed to run 'systemctl daemon-reload': %w. Output: %s", errDaemonReload, string(outputDaemonReload))
	}
	zlog.Debug().Bytes("output", outputDaemonReload).Msg("systemctl daemon-reload executed")

	zlog.Info().Msg("Systemd service removed.")
	return nil
}


// --- Monitoring Core ---
func (item *MonitoredItem) updateDiskStatus() {
	item.LastCheckTime = time.Now()
	if item.DiskConfig == nil { item.Status = StatusUnknown; item.Details = "Disk configuration missing"; return }
	var stat syscall.Statfs_t
	err := syscall.Statfs(item.PathOrURL, &stat)
	if err != nil { item.Status = StatusUnhealthy; item.Details = fmt.Sprintf("Failed to get disk stats: %v", err); return }
	usagePercent := (float64(stat.Blocks-stat.Bfree) / float64(stat.Blocks)) * 100
	item.Details = fmt.Sprintf("%.2f%% used", usagePercent)
	item.Status = StatusHealthy; if usagePercent >= item.Threshold { item.Status = StatusUnhealthy }
}

func (item *MonitoredItem) updateCPUStatus() {
	item.LastCheckTime = time.Now()
	if item.CPUConfig == nil || !item.CPUConfig.Enabled { item.Status = StatusUnknown; item.Details = "CPU monitoring disabled or config missing"; return }
	percentages, err := cpu.Percent(time.Second, false)
	if err != nil { item.Status = StatusUnhealthy; item.Details = fmt.Sprintf("Failed to get CPU usage: %v", err); return }
	if len(percentages) == 0 { item.Status = StatusUnhealthy; item.Details = "No CPU usage data returned"; return }
	cpuUsage := percentages[0]
	item.Details = fmt.Sprintf("%.2f%% usage", cpuUsage)
	item.Status = StatusHealthy; if cpuUsage >= item.Threshold { item.Status = StatusUnhealthy }
}

func (item *MonitoredItem) updateMemoryStatus() {
	item.LastCheckTime = time.Now()
	if item.MemoryConfig == nil || !item.MemoryConfig.Enabled { item.Status = StatusUnknown; item.Details = "Memory monitoring disabled or config missing"; return }
	vmStat, err := mem.VirtualMemory()
	if err != nil { item.Status = StatusUnhealthy; item.Details = fmt.Sprintf("Failed to get memory stats: %v", err); return }
	memUsage := vmStat.UsedPercent
	item.Details = fmt.Sprintf("%.2f%% used", memUsage)
	item.Status = StatusHealthy; if memUsage >= item.Threshold { item.Status = StatusUnhealthy }
}

func (item *MonitoredItem) updateAppStatus() {
	item.LastCheckTime = time.Now()
	if item.AppConfig == nil { item.Status = StatusUnknown; item.Details = "Application configuration missing"; return }
	appCfg := item.AppConfig
	client := http.Client{Timeout: time.Duration(appCfg.TimeoutSeconds) * time.Second}
	startTime := time.Now()
	resp, err := client.Get(item.PathOrURL)
	responseTime := time.Since(startTime)
	if err != nil { item.Status = StatusUnhealthy; item.Details = fmt.Sprintf("Error GET %s: %v", item.PathOrURL, err); return }
	defer resp.Body.Close()
	currentDetails := fmt.Sprintf("Status Code: %d, Resp Time: %s", resp.StatusCode, responseTime.Round(time.Millisecond))
	if resp.StatusCode != appCfg.ExpectedStatusCode { item.Status = StatusUnhealthy; item.Details = currentDetails + fmt.Sprintf(" (Expected %d)", appCfg.ExpectedStatusCode); return }
	if appCfg.Threshold.MaxResponseTimeMS > 0 && responseTime.Milliseconds() > int64(appCfg.Threshold.MaxResponseTimeMS) { item.Status = StatusUnhealthy; item.Details = currentDetails + fmt.Sprintf(" (Resp time %dms > threshold %dms)", responseTime.Milliseconds(), appCfg.Threshold.MaxResponseTimeMS); return }
	if appCfg.Threshold.BodyContains != "" || appCfg.Threshold.BodyNotContains != "" {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr != nil { item.Status = StatusUnhealthy; item.Details = currentDetails + fmt.Sprintf(" (Error reading body: %v)", readErr); return }
		bodyString := string(bodyBytes)
		if appCfg.Threshold.BodyContains != "" && !strings.Contains(bodyString, appCfg.Threshold.BodyContains) { item.Status = StatusUnhealthy; item.Details = currentDetails + fmt.Sprintf(" (Body missing: '%s')", appCfg.Threshold.BodyContains); return }
		if appCfg.Threshold.BodyNotContains != "" && strings.Contains(bodyString, appCfg.Threshold.BodyNotContains) { item.Status = StatusUnhealthy; item.Details = currentDetails + fmt.Sprintf(" (Body contains unwanted: '%s')", appCfg.Threshold.BodyNotContains); return }
	}
	item.Status = StatusHealthy; item.Details = currentDetails
}

func getFaviconURL(appURL string) (string, error) {
	parsedURL, err := url.Parse(appURL)
	if err != nil { return "", fmt.Errorf("could not parse app URL %s: %w", appURL, err) }
	return fmt.Sprintf("%s://%s/favicon.ico", parsedURL.Scheme, parsedURL.Host), nil
}

func sendWebhookNotification(webhookCfg WebhookConfig, item MonitoredItem) error {
	payload := map[string]interface{}{"itemName": item.Name, "itemType": item.Type, "status": item.Status, "details": item.Details, "lastCheckTime": item.LastCheckTime.Format(time.RFC3339), "threshold": item.Threshold, "pathOrURL": item.PathOrURL}
	jsonPayload, err := json.Marshal(payload)
	if err != nil { return fmt.Errorf("failed to marshal JSON payload for webhook %s: %w", webhookCfg.Name, err) }
	req, err := http.NewRequest(strings.ToUpper(webhookCfg.Method), webhookCfg.URL, bytes.NewBuffer(jsonPayload))
	if err != nil { return fmt.Errorf("failed to create request for webhook %s: %w", webhookCfg.Name, err) }
	req.Header.Set("Content-Type", "application/json")
	for key, value := range webhookCfg.Headers { req.Header.Set(key, value) }
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil { return fmt.Errorf("failed to send webhook %s: %w", webhookCfg.Name, err) }
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 { zlog.Info().Str("webhook_name", webhookCfg.Name).Str("item_name", item.Name).Int("status_code", resp.StatusCode).Msg("Webhook sent successfully"); return nil }
	responseBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("webhook %s for item %s failed with status %d: %s", webhookCfg.Name, item.Name, resp.StatusCode, string(responseBody))
}

func initializeMonitoredItems() {
	MonitoredItems = []*MonitoredItem{}
	if Cfg.CPU.Enabled { MonitoredItems = append(MonitoredItems, &MonitoredItem{Name: "CPU Usage", Type: TypeCPU, Status: StatusUnknown, PreviousStatus: StatusUnknown, Threshold: Cfg.CPU.Threshold, Icon: Cfg.CPU.Icon, CPUConfig: &Cfg.CPU}) }
	if Cfg.Memory.Enabled { MonitoredItems = append(MonitoredItems, &MonitoredItem{Name: "Memory Usage", Type: TypeMemory, Status: StatusUnknown, PreviousStatus: StatusUnknown, Threshold: Cfg.Memory.Threshold, Icon: Cfg.Memory.Icon, MemoryConfig: &Cfg.Memory}) }
	for i := range Cfg.Disks { diskCfg := Cfg.Disks[i]; MonitoredItems = append(MonitoredItems, &MonitoredItem{Name: diskCfg.Name, Type: TypeDisk, Status: StatusUnknown, PreviousStatus: StatusUnknown, PathOrURL: diskCfg.Path, Threshold: diskCfg.Threshold, Icon: diskCfg.Icon, DiskConfig: &diskCfg}) }
	for i := range Cfg.Applications {
		appCfg := Cfg.Applications[i]
		item := &MonitoredItem{Name: appCfg.Name, Type: TypeApplication, Status: StatusUnknown, PreviousStatus: StatusUnknown, PathOrURL: appCfg.URL, Icon: appCfg.Icon, AppConfig: &appCfg}
		if item.Icon == "" && appCfg.IconFetch {
			faviconURL, err := getFaviconURL(appCfg.URL)
			if err == nil { item.Icon = faviconURL; zlog.Debug().Str("app", appCfg.Name).Str("favicon", faviconURL).Msg("Attempting to use auto-fetched favicon")
			} else { zlog.Warn().Err(err).Str("app", appCfg.Name).Msg("Could not determine favicon URL") }
		}
		MonitoredItems = append(MonitoredItems, item)
	}
}

func runAllChecks() {
	itemsMutex.Lock(); defer itemsMutex.Unlock()
	zlog.Info().Msg("Running all monitoring checks...")
	for _, item := range MonitoredItems {
		switch item.Type {
		case TypeCPU: item.updateCPUStatus()
		case TypeMemory: item.updateMemoryStatus()
		case TypeDisk: item.updateDiskStatus()
		case TypeApplication: item.updateAppStatus()
		default: item.Status = StatusUnknown; item.Details = "Unknown monitor type"; zlog.Warn().Str("item_name", item.Name).Msg("Unknown monitor type")
		}
		zlog.Debug().Str("item_name", item.Name).Str("type", string(item.Type)).Str("status", string(item.Status)).Str("prev_status", string(item.PreviousStatus)).Str("details", item.Details).Msg("Checked item")
		if item.Status == StatusUnhealthy && item.PreviousStatus != StatusUnhealthy {
			if len(Cfg.Webhooks) > 0 {
				zlog.Info().Str("item_name", item.Name).Msg("Item changed to UNHEALTHY, sending notifications...")
				for _, webhookCfg := range Cfg.Webhooks {
					if webhookCfg.URL != "" && webhookCfg.URL != "https_your_slack_webhook_url_here" {
						if err := sendWebhookNotification(webhookCfg, *item); err != nil { zlog.Error().Err(err).Str("webhook_name", webhookCfg.Name).Str("item_name", item.Name).Msg("Error sending webhook") }
					} else { zlog.Warn().Str("webhook_name", webhookCfg.Name).Str("item_name", item.Name).Msg("Skipping webhook due to invalid/placeholder URL") }
				}
			} else { zlog.Info().Str("item_name", item.Name).Msg("Item changed to UNHEALTHY, but no webhooks configured") }
		}
		item.PreviousStatus = item.Status
	}
	zlog.Info().Msg("All checks complete.")
}

type statusPageData struct { Items []*MonitoredItem; Timestamp time.Time; RefreshIntervalSeconds int; IconBaseURL string }
func replaceNonAlphaNum(s string) string { return strings.Map(func(r rune) rune { if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') { return r }; return '-' }, s) }

func statusPageHandler(w http.ResponseWriter, r *http.Request) {
	itemsMutex.RLock()
	currentItemsSnapshot := make([]*MonitoredItem, 0, len(MonitoredItems))
	for _, item := range MonitoredItems { copiedItem := *item; currentItemsSnapshot = append(currentItemsSnapshot, &copiedItem) }
	itemsMutex.RUnlock()
	data := statusPageData{Items: currentItemsSnapshot, Timestamp: time.Now(), RefreshIntervalSeconds: Cfg.MonitorIntervalSeconds, IconBaseURL: Cfg.Server.IconBaseURL}
	if parsedStatusTemplate == nil { zlog.Error().Msg("Status template not parsed"); http.Error(w, "Internal Server Error: Template not ready", http.StatusInternalServerError); return }
	if err := parsedStatusTemplate.Execute(w, data); err != nil { zlog.Error().Err(err).Msg("Error executing status page template"); http.Error(w, "Internal Server Error", http.StatusInternalServerError) }
}

type configPageData struct { Config *Config; ErrorMessage string; SuccessMessage string }
func configPageHandler(w http.ResponseWriter, r *http.Request) {
	data := configPageData{Config: &Cfg}
	if parsedConfigTemplate == nil { zlog.Error().Msg("Config template not parsed"); http.Error(w, "Internal Server Error: Template not ready", http.StatusInternalServerError); return }
	if err := parsedConfigTemplate.Execute(w, data); err != nil { zlog.Error().Err(err).Msg("Error executing config page template"); http.Error(w, "Internal Server Error", http.StatusInternalServerError) }
}

func saveConfig() error {
	itemsMutex.Lock(); defer itemsMutex.Unlock()
	// Ensure config directory exists using viper's knowledge or default.
	configFilePath := viper.ConfigFileUsed()
	var configDir string
	if configFilePath != "" {
		configDir = filepath.Dir(configFilePath)
	} else {
		// If no config file is used by viper, attempt to use the first path in viper.ConfigPaths()
		// or default to "./config"
		paths := viper.ConfigPaths()
		if len(paths) > 0 {
			configDir = paths[0]
		} else {
			configDir = "./config" // Fallback default
		}
		configFilePath = filepath.Join(configDir, "config.yaml") // Ensure configFilePath is set
	}

	if _, err := os.Stat(configDir); os.IsNotExist(err) {
		if mkErr := os.MkdirAll(configDir, 0755); mkErr != nil {
			return fmt.Errorf("failed to create config directory %s: %w", configDir, mkErr)
		}
	}

	zlog.Info().Str("path", configFilePath).Msg("Attempting to save configuration")
	yamlData, err := yaml.Marshal(&Cfg)
	if err != nil { return fmt.Errorf("failed to marshal config to YAML: %w", err) }
	if err = os.WriteFile(configFilePath, yamlData, 0644); err != nil { return fmt.Errorf("failed to write config file %s: %w", configFilePath, err) }
	zlog.Info().Str("path", configFilePath).Msg("Configuration saved successfully")
	initializeMonitoredItems() // This needs to be within the same mutex lock if it modifies MonitoredItems
	zlog.Info().Msg("Monitored items re-initialized after config change.")
	return nil
}

func handleAddDisk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed); return }
	r.ParseForm(); name := r.FormValue("name"); path := r.FormValue("path"); thresholdStr := r.FormValue("threshold"); icon := r.FormValue("icon")
	if name == "" || path == "" || thresholdStr == "" { http.Error(w, "Missing required fields for disk monitor", http.StatusBadRequest); return }
	threshold, err := strconv.ParseFloat(thresholdStr, 64); if err != nil { http.Error(w, "Invalid threshold value", http.StatusBadRequest); return }
	newDisk := DiskMonitorConfig{Name: name, Path: path, Threshold: threshold, Icon: icon}
	itemsMutex.Lock(); Cfg.Disks = append(Cfg.Disks, newDisk); itemsMutex.Unlock()
	if err := saveConfig(); err != nil { zlog.Error().Err(err).Msg("Error saving config after adding disk"); http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError); return }
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func handleRemoveDisk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed); return }
	r.ParseForm(); pathToRemove := r.FormValue("path")
	if pathToRemove == "" { http.Error(w, "Missing path for disk removal", http.StatusBadRequest); return }
	itemsMutex.Lock(); newDisks := []DiskMonitorConfig{}; found := false
	for _, disk := range Cfg.Disks { if disk.Path == pathToRemove { found = true; continue }; newDisks = append(newDisks, disk) }
	if found { Cfg.Disks = newDisks }
	itemsMutex.Unlock()
	if !found { zlog.Warn().Str("path", pathToRemove).Msg("Attempted to remove disk, but not found"); http.Redirect(w, r, "/config", http.StatusSeeOther); return }
	if err := saveConfig(); err != nil { zlog.Error().Err(err).Msg("Error saving config after removing disk"); http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError); return }
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func handleRemoveApplication(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed); return }
	r.ParseForm(); nameToRemove := r.FormValue("name")
	if nameToRemove == "" { http.Error(w, "Missing name for application removal", http.StatusBadRequest); return }
	itemsMutex.Lock(); newApps := []AppMonitorConfig{}; found := false
	for _, app := range Cfg.Applications { if app.Name == nameToRemove { found = true; continue }; newApps = append(newApps, app) }
	if found { Cfg.Applications = newApps }
	itemsMutex.Unlock()
	if !found { zlog.Warn().Str("name", nameToRemove).Msg("Attempted to remove application, but not found"); http.Redirect(w, r, "/config", http.StatusSeeOther); return }
	if err := saveConfig(); err != nil { zlog.Error().Err(err).Msg("Error saving config after removing application"); http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError); return }
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func handleAddApplication(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost { http.Error(w, "Only POST method is allowed", http.StatusMethodNotAllowed); return }
	r.ParseForm(); name := r.FormValue("name"); appURL := r.FormValue("url"); expectedStatusCodeStr := r.FormValue("expectedStatusCode"); timeoutSecondsStr := r.FormValue("timeoutSeconds"); icon := r.FormValue("icon"); iconFetch := r.FormValue("iconFetch") == "true"; maxResponseTimeMSStr := r.FormValue("threshold.maxResponseTimeMS"); bodyContains := r.FormValue("threshold.bodyContains"); bodyNotContains := r.FormValue("threshold.bodyNotContains")
	if name == "" || appURL == "" || expectedStatusCodeStr == "" || timeoutSecondsStr == "" { http.Error(w, "Missing required fields for application monitor", http.StatusBadRequest); return }
	expectedStatusCode, err := strconv.Atoi(expectedStatusCodeStr); if err != nil { http.Error(w, "Invalid expected status code", http.StatusBadRequest); return }
	timeoutSeconds, err := strconv.Atoi(timeoutSecondsStr); if err != nil { http.Error(w, "Invalid timeout seconds", http.StatusBadRequest); return }
	var maxResponseTimeMS int
	if maxResponseTimeMSStr != "" { maxResponseTimeMS, err = strconv.Atoi(maxResponseTimeMSStr); if err != nil { http.Error(w, "Invalid max response time", http.StatusBadRequest); return } }
	newApp := AppMonitorConfig{ Name: name, URL: appURL, ExpectedStatusCode: expectedStatusCode, TimeoutSeconds: timeoutSeconds, Icon: icon, IconFetch: iconFetch, Threshold: AppThresholdConfig{MaxResponseTimeMS: maxResponseTimeMS, BodyContains: bodyContains, BodyNotContains: bodyNotContains}}
	itemsMutex.Lock(); Cfg.Applications = append(Cfg.Applications, newApp); itemsMutex.Unlock()
	if err := saveConfig(); err != nil { zlog.Error().Err(err).Msg("Error saving config after adding application"); http.Error(w, fmt.Sprintf("Failed to save config: %v", err), http.StatusInternalServerError); return }
	http.Redirect(w, r, "/config", http.StatusSeeOther)
}

func main() {
	installServiceFlag := flag.Bool("install-service", false, "Install the application as a systemd service (requires sudo).")
	removeServiceFlag := flag.Bool("remove-service", false, "Remove the systemd service (requires sudo).")
	configDirFlag := flag.String("config-dir", "./config", "Directory to load config.yaml from. Viper will also check '.'")
	flag.Parse()

	zlog.Logger = zlog.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	zerolog.SetGlobalLevel(zerolog.InfoLevel) // Default level before config is read

	if *installServiceFlag {
		// Pass the effective config directory the service should use.
		// If running `go-monitor -install-service -config-dir /custom/path`, it uses that.
		// Otherwise, it uses the default "./config" relative to where the service will run.
		if err := installSystemdService(*configDirFlag); err != nil { zlog.Fatal().Err(err).Msg("Failed to install systemd service") }
		zlog.Info().Msg("Systemd service installation process finished."); os.Exit(0)
	}
	if *removeServiceFlag {
		if err := removeSystemdService(); err != nil { zlog.Fatal().Err(err).Msg("Failed to remove systemd service") }
		zlog.Info().Msg("Systemd service removal process finished."); os.Exit(0)
	}

	var err error
	funcMap := template.FuncMap{"replaceNonAlphaNum": replaceNonAlphaNum, "eq": func(a,b interface{}) bool { return a==b }, "hasPrefix": func(s,p string) bool { return strings.HasPrefix(s,p) }}
	parsedStatusTemplate, err = template.New("status.html").Funcs(funcMap).ParseFS(templatesFS, "templates/status.html")
	if err != nil { zlog.Fatal().Err(err).Msg("Error parsing status.html template") } // zlog is now initialized
	parsedConfigTemplate, err = template.New("config.html").Funcs(funcMap).ParseFS(templatesFS, "templates/config.html")
	if err != nil { zlog.Fatal().Err(err).Msg("Error parsing config.html template") }

	viper.SetConfigName("config"); viper.SetConfigType("yaml")
	viper.AddConfigPath(*configDirFlag)
	viper.AddConfigPath("."); viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_")); viper.AutomaticEnv()

	viper.SetDefault("server.host", "0.0.0.0"); viper.SetDefault("server.port", 8080); viper.SetDefault("server.iconBaseURL", "/static/icons/")
	viper.SetDefault("cpu.enabled", true); viper.SetDefault("cpu.threshold", 90.0); viper.SetDefault("cpu.icon", "cpu.svg")
	viper.SetDefault("memory.enabled", true); viper.SetDefault("memory.threshold", 90.0); viper.SetDefault("memory.icon", "memory.svg")
	viper.SetDefault("monitorIntervalSeconds", 10); viper.SetDefault("logging.format", "text"); viper.SetDefault("logging.level", "info")

	// stdlog is still used for viper's own internal pre-unmarshal errors, if any, before zlog is fully configured by Cfg
	stdlog.SetFlags(stdlog.LstdFlags | stdlog.Lshortfile)

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Log with stdlog because zlog format/level isn't set from config yet
			stdlog.Printf("Config file not found in %s or '.', using defaults/env vars. This is not an error if intended.", *configDirFlag)
		} else { stdlog.Fatalf("Error reading config file: %s", err) }
	}
	if err := viper.Unmarshal(&Cfg); err != nil { stdlog.Fatalf("Unable to decode config into struct: %v", err) }

	// Re-configure global zerolog logger based on loaded Cfg
	if strings.ToLower(Cfg.Logging.Format) == "json" {
		zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
		zlog.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	} else {
		zlog.Logger = zlog.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	}
	logLevel, err := zerolog.ParseLevel(strings.ToLower(Cfg.Logging.Level))
	if err != nil {
		zlog.Warn().Str("configuredLevel", Cfg.Logging.Level).Err(err).Msg("Invalid log level, defaulting to info")
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	} else { zerolog.SetGlobalLevel(logLevel) }

	zlog.Info().Msg("Go Monitor Application Starting..."); zlog.Debug().Interface("config", &Cfg).Msg("Loaded Configuration")
	initializeMonitoredItems(); runAllChecks()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM); defer stop()
	mux := http.NewServeMux()
	mux.HandleFunc("/", statusPageHandler); mux.HandleFunc("/config", configPageHandler)
	mux.HandleFunc("/config/add/disk", handleAddDisk); mux.HandleFunc("/config/add/application", handleAddApplication)
	mux.HandleFunc("/config/remove/disk", handleRemoveDisk); mux.HandleFunc("/config/remove/application", handleRemoveApplication)
	staticFS, err := fs.Sub(staticFilesFS, "static"); if err != nil { zlog.Fatal().Err(err).Msg("Failed to get sub FS for static files") }
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	serverAddr := fmt.Sprintf("%s:%d", Cfg.Server.Host, Cfg.Server.Port)
	httpServer := &http.Server{Addr: serverAddr, Handler: mux}

	go func() { zlog.Info().Str("address", serverAddr).Msg("Starting HTTP server"); if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed { zlog.Fatal().Err(err).Msg("HTTP server ListenAndServe failed") } }()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		if Cfg.MonitorIntervalSeconds > 0 {
			ticker := time.NewTicker(time.Duration(Cfg.MonitorIntervalSeconds) * time.Second); defer ticker.Stop()
			zlog.Info().Int("interval_seconds", Cfg.MonitorIntervalSeconds).Msg("Starting monitoring loop")
			for { select { case <-ticker.C: runAllChecks(); case <-ctx.Done(): zlog.Info().Msg("Monitoring loop: Shutdown signal received, stopping..."); return } }
		} else { zlog.Info().Msg("No periodic monitoring configured. Monitoring loop will not run.") }
	}()

	<-ctx.Done(); zlog.Info().Msg("Shutdown signal received, initiating graceful shutdown...")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second); defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil { zlog.Error().Err(err).Msg("HTTP server Shutdown error")
	} else { zlog.Info().Msg("HTTP server gracefully stopped.") }
	if Cfg.MonitorIntervalSeconds > 0 { zlog.Info().Msg("Waiting for monitoring loop to complete..."); <-monitorDone; zlog.Info().Msg("Monitoring loop completed.") }
	zlog.Info().Msg("Application shut down gracefully.")
}
