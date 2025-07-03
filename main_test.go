package main

import (
	"testing"
)

func TestReplaceNonAlphaNum(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty string", "", ""},
		{"all alpha", "HelloWorld", "HelloWorld"},
		{"with numbers", "Version123", "Version123"},
		{"with spaces", "Hello World", "Hello-World"},
		{"with hyphens", "hello-world", "hello-world"},
		{"with underscores", "hello_world", "hello_world"},
		{"with special chars", "Name!@#$%^&*()_+", "Name----------"},
		{"mixed case and special", "My-App_v1.0!", "My-App_v1-0-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replaceNonAlphaNum(tt.input); got != tt.expected {
				t.Errorf("replaceNonAlphaNum(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// Helper to initialize templates for tests, similar to main()
func setupTemplatesForTest(t *testing.T) {
	t.Helper()
	var err error
	funcMap := template.FuncMap{
		"replaceNonAlphaNum": replaceNonAlphaNum,
		"eq":                 func(a, b interface{}) bool { return a == b },
		"hasPrefix":          func(s, prefix string) bool { return strings.HasPrefix(s, prefix) },
	}
	// Assume templatesFS is available or mock it if necessary for pure unit tests
	// For this test, we rely on the embedded FS.
	parsedStatusTemplate, err = template.New("status.html").Funcs(funcMap).ParseFS(templatesFS, "templates/status.html")
	if err != nil {
		t.Fatalf("Error parsing status.html template for test: %v", err)
	}
	// Config template might not be needed for status page handler test
}


func TestStatusPageHandler(t *testing.T) {
	setupTemplatesForTest(t)

	// Sample MonitoredItems for testing
	originalMonitoredItems := MonitoredItems // Save original
	MonitoredItems = []*MonitoredItem{
		{Name: "Test CPU", Type: TypeCPU, Status: StatusHealthy, Details: "10% usage", LastCheckTime: time.Now()},
		{Name: "Test Disk", Type: TypeDisk, Status: StatusUnhealthy, Details: "90% used", LastCheckTime: time.Now(), Threshold: 80},
	}
	defer func() { MonitoredItems = originalMonitoredItems }() // Restore original

	// Mock Cfg for IconBaseURL if needed, or set directly for test scope
	originalCfg := Cfg
	Cfg.Server.IconBaseURL = "/static/icons/"
	Cfg.MonitorIntervalSeconds = 15 // For refresh interval in template
	defer func() { Cfg = originalCfg }()


	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(statusPageHandler)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	// Check for some expected content
	expectedContent := "System Status Dashboard"
	if !strings.Contains(rr.Body.String(), expectedContent) {
		t.Errorf("handler returned unexpected body: got %v want to contain %q", rr.Body.String(), expectedContent)
	}
	expectedItem1 := "Test CPU"
	if !strings.Contains(rr.Body.String(), expectedItem1) {
		t.Errorf("handler returned unexpected body: got %v want to contain %q", rr.Body.String(), expectedItem1)
	}
	expectedItem2 := "Test Disk"
	if !strings.Contains(rr.Body.String(), expectedItem2) {
		t.Errorf("handler returned unexpected body: got %v want to contain %q", rr.Body.String(), expectedItem2)
	}
}


// TODO: Add more tests for:
// - Configuration loading (Viper setup)
// - Monitoring logic (getDiskUsage, getCPUUsage, getMemoryUsage, checkApplicationHealth - may require mocks/stubs)
// - HTTP handlers (statusPageHandler, configPageHandler, add/remove handlers - using httptest)
// - Favicon URL generation (getFaviconURL)
// - Health status determination based on thresholds
// - Webhook payload generation (if specific structure is important)
// - Systemd service file generation (if made more dynamic)
// - Graceful shutdown sequence (more complex to test, might be integration)
