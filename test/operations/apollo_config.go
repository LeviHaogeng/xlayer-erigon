package operations

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/apolloconfig/agollo/v4"
	"github.com/apolloconfig/agollo/v4/env/config"
)

const (
	// Apollo configuration constants
	apolloConfigURL     = "127.0.0.1:8085"       // Apollo config service url
	apolloSeqAppID      = "XLayerSeq"            // Default Apollo app ID
	apolloPoolNamespace = "pool-config.txt"      // Pool namespace for txpool config
	apolloSeqNamespace  = "sequencer-config.txt" // Sequencer namespace for sequencer config

	// Apollo authentication
	apolloUsername = "apollo" // Apollo admin username
	apolloPassword = "admin"  // Apollo admin password

	// HugeTx e2e test configuration keys - Pool namespace
	hugeTxThresholdRatioKey      = "txpool.huge-tx-threshold-ratio"       // Percentage threshold to identify huge transactions (0-100)
	hugeTxQuotaRatioKey          = "txpool.huge-tx-quota-ratio"           // Maximum percentage of block gas consumed by huge transactions (0-100)
	hugeTxIgnoreQuotaIntervalKey = "txpool.ignore-huge-tx-quota-interval" // Block interval to ignore huge transaction quota restrictions
	hugeTxYieldKey               = "hugeTxE2EYieldEnabled"                // Key for controlling transaction yielding

	// HugeTx e2e test configuration keys - Sequencer namespace
	dynamicBlockGasLimitKey = "zkevm.dynamic-block-gas-limit" // Dynamic block gas limit for zkevm
)

// ApolloConfigController manages Apollo configuration changes for testing
type ApolloConfigController struct {
	client     *agollo.Client
	t          *testing.T
	cookieFile string // Store session cookie file path
}

// NewApolloConfigController creates a new Apollo config controller for testing
func NewApolloConfigController(t *testing.T) *ApolloConfigController {
	// Create Apollo client configuration (for reading config changes)
	c := &config.AppConfig{
		IP:             apolloConfigURL, // Config service port
		AppID:          apolloSeqAppID,
		NamespaceName:  apolloPoolNamespace,
		Cluster:        "default",
		IsBackupConfig: false,
	}

	// Start Apollo client
	client, err := agollo.StartWithConfig(func() (*config.AppConfig, error) {
		return c, nil
	})
	if err != nil {
		t.Logf("Failed to create Apollo client (config may not be available): %v", err)
		return nil // Return nil if Apollo is not available, tests can still run
	}

	// Initialize session for Portal API
	ctrl := &ApolloConfigController{
		client:     client,
		t:          t,
		cookieFile: fmt.Sprintf("/tmp/apollo_cookies_%d.txt", time.Now().UnixNano()),
	}

	// Login to Portal to get session cookie
	if err := ctrl.initPortalSession(); err != nil {
		t.Logf("Failed to initialize Apollo Portal session: %v", err)
		return nil
	}

	return ctrl
}

// initPortalSession logs into Apollo Portal and saves session cookie
func (c *ApolloConfigController) initPortalSession() error {
	// Create client with no redirect following to capture cookies from 302 response
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	// Login to Apollo Portal
	loginData := fmt.Sprintf("username=%s&password=%s", apolloUsername, apolloPassword)
	req, err := http.NewRequest("POST", "http://127.0.0.1:8070/signin", bytes.NewBuffer([]byte(loginData)))
	if err != nil {
		return fmt.Errorf("failed to create login request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to login: %v", err)
	}
	defer resp.Body.Close()

	// Check for successful login (302 redirect with session cookie)
	if resp.StatusCode != 302 {
		return fmt.Errorf("login failed with status: %d", resp.StatusCode)
	}

	// Save cookies to file for later use
	if len(resp.Cookies()) == 0 {
		return fmt.Errorf("no session cookie received after login")
	}

	// Create cookie file content
	var cookieContent bytes.Buffer
	for _, cookie := range resp.Cookies() {
		cookieContent.WriteString("# Netscape HTTP Cookie File\n")
		cookieContent.WriteString(fmt.Sprintf("127.0.0.1\tFALSE\t/\tFALSE\t0\t%s\t%s\n", cookie.Name, cookie.Value))
	}

	if err := os.WriteFile(c.cookieFile, cookieContent.Bytes(), 0600); err != nil {
		return fmt.Errorf("failed to save cookies: %v", err)
	}

	c.t.Logf("Successfully logged into Apollo Portal with %d cookies", len(resp.Cookies()))
	return nil
}

// getNamespaceData gets namespace data including item IDs for a specific namespace
func (c *ApolloConfigController) getNamespaceData(namespace string) (*NamespaceData, error) {
	if c == nil || c.client == nil {
		return nil, fmt.Errorf("apollo client not available")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:8070/apps/XLayerSeq/envs/DEV/clusters/default/namespaces/%s", namespace)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %v", err)
	}

	// Load cookies from file
	if cookies, err := c.loadCookies(); err == nil {
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to get config, status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	// Parse the JSON response
	var nsData NamespaceData
	if err := json.Unmarshal(body, &nsData); err != nil {
		return nil, fmt.Errorf("failed to parse config: %v", err)
	}

	return &nsData, nil
}

// NamespaceData represents the structure of Apollo namespace data
type NamespaceData struct {
	Items []NamespaceItem `json:"items"`
}

type NamespaceItem struct {
	Item struct {
		ID    int    `json:"id"`
		Value string `json:"value"`
	} `json:"item"`
}

// loadCookies loads cookies from file
func (c *ApolloConfigController) loadCookies() ([]*http.Cookie, error) {
	data, err := os.ReadFile(c.cookieFile)
	if err != nil {
		return nil, err
	}

	var cookies []*http.Cookie
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("#")) || len(line) == 0 {
			continue
		}
		parts := bytes.Split(line, []byte("\t"))
		if len(parts) >= 7 {
			cookie := &http.Cookie{
				Name:  string(parts[5]),
				Value: string(parts[6]),
			}
			cookies = append(cookies, cookie)
		}
	}
	return cookies, nil
}

// updateNamespaceConfig updates the configuration content for a specific namespace
func (c *ApolloConfigController) updateNamespaceConfig(namespace, newContent string) error {
	if c == nil || c.client == nil {
		c.t.Logf("Apollo client not available, skipping configuration change")
		return nil
	}

	client := &http.Client{Timeout: 10 * time.Second}

	// Get current config to get the item ID
	currentNSData, err := c.getNamespaceData(namespace)
	if err != nil {
		return fmt.Errorf("failed to get current namespace data: %v", err)
	}

	if len(currentNSData.Items) == 0 {
		return fmt.Errorf("no config items found in namespace")
	}

	// Create update payload with existing item ID
	payload := map[string]interface{}{
		"id":      currentNSData.Items[0].Item.ID,
		"key":     "content",
		"value":   newContent,
		"comment": "Updated by E2E test",
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal payload: %v", err)
	}

	// Update config item using correct endpoint
	url := fmt.Sprintf("http://127.0.0.1:8070/apps/XLayerSeq/envs/DEV/clusters/default/namespaces/%s/item", namespace)
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Load cookies from file
	if cookies, err := c.loadCookies(); err == nil {
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to update config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("update failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Publish changes
	return c.publishNamespaceConfig(namespace)
}

// publishNamespaceConfig publishes the config changes for a specific namespace
func (c *ApolloConfigController) publishNamespaceConfig(namespace string) error {
	client := &http.Client{Timeout: 10 * time.Second}

	payload := map[string]interface{}{
		"releaseTitle":   "E2E Test Release",
		"releaseComment": "Published by E2E test",
		"releasedBy":     "test",
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal release payload: %v", err)
	}

	url := fmt.Sprintf("http://127.0.0.1:8070/apps/XLayerSeq/envs/DEV/clusters/default/namespaces/%s/releases", namespace)
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create release request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	// Load cookies from file
	if cookies, err := c.loadCookies(); err == nil {
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to publish config: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("publish failed with status %d: %s", resp.StatusCode, string(body))
	}

	c.t.Logf("Successfully published %s config changes to Apollo", namespace)
	return nil
}

// getNamespaceForKey returns the appropriate namespace for a given configuration key
func (c *ApolloConfigController) getNamespaceForKey(key string) string {
	switch key {
	case dynamicBlockGasLimitKey:
		return apolloSeqNamespace
	case hugeTxThresholdRatioKey, hugeTxQuotaRatioKey, hugeTxIgnoreQuotaIntervalKey, hugeTxYieldKey:
		return apolloPoolNamespace
	default:
		// Default to pool namespace for unknown keys
		return apolloPoolNamespace
	}
}

// clearNamespaceContent clears all content in a namespace by resetting to empty
func (c *ApolloConfigController) clearNamespaceContent(namespace string) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("apollo client not available")
	}

	c.t.Logf("Clearing all content in namespace %s", namespace)

	// Simply set empty content to clear everything
	if err := c.updateNamespaceConfig(namespace, ""); err != nil {
		return fmt.Errorf("failed to clear namespace %s: %v", namespace, err)
	}

	c.t.Logf("Successfully cleared namespace %s", namespace)
	return nil
}

// setConfigKey sets a configuration key-value pair in the appropriate namespace
func (c *ApolloConfigController) setConfigKey(key, value, description string) {
	if c == nil || c.client == nil {
		c.t.Logf("Apollo client not available, skipping %s configuration change", description)
		return
	}

	namespace := c.getNamespaceForKey(key)
	c.t.Logf("Setting %s = %s via Apollo in namespace %s (clearing namespace first)", key, value, namespace)

	// Clear namespace content first to remove any unwanted configurations
	if err := c.clearNamespaceContent(namespace); err != nil {
		c.t.Logf("Failed to clear namespace %s: %v", namespace, err)
		return
	}

	// Set only the key we want
	newConfig := fmt.Sprintf("%s: %s\n", key, value)

	// Update the entire config
	if err := c.updateNamespaceConfig(namespace, newConfig); err != nil {
		c.t.Logf("Failed to update %s config: %v", namespace, err)
		return
	}

	// Wait for configuration to propagate to all instances
	c.t.Logf("Successfully set %s = %s in namespace %s", key, value, namespace)
}

// SetHugeTxYieldEnabled publishes hugeTxE2EYieldEnabled configuration to Apollo
func (c *ApolloConfigController) SetHugeTxYieldEnabled(enabled bool) {
	if c == nil || c.client == nil {
		c.t.Logf("Apollo client not available, skipping HugeTx yield enabled configuration change")
		return
	}

	value := "false"
	if enabled {
		value = "true"
	}

	namespace := apolloPoolNamespace
	c.t.Logf("Setting %s = %s in namespace %s", hugeTxYieldKey, value, namespace)

	// Create new config with only the yield enabled setting (don't preserve old configs)
	newConfig := fmt.Sprintf("%s: %s\n", hugeTxYieldKey, value)

	// Update the entire config
	if err := c.updateNamespaceConfig(namespace, newConfig); err != nil {
		c.t.Logf("Failed to update %s config: %v", namespace, err)
		return
	}

	c.t.Logf("Successfully set %s = %s in namespace %s", hugeTxYieldKey, value, namespace)
}

// SetHugeTxThresholdRatio sets the percentage threshold to identify huge transactions (0-100)
func (c *ApolloConfigController) SetHugeTxThresholdRatio(ratio uint64) {
	c.setUint64Config(hugeTxThresholdRatioKey, ratio, "HugeTx threshold ratio")
}

// SetHugeTxQuotaRatio sets the maximum percentage of block gas consumed by huge transactions (0-100)
func (c *ApolloConfigController) SetHugeTxQuotaRatio(ratio uint64) {
	c.setUint64Config(hugeTxQuotaRatioKey, ratio, "HugeTx quota ratio")
}

// SetHugeTxIgnoreQuotaInterval sets the block interval to ignore huge transaction quota restrictions
func (c *ApolloConfigController) SetHugeTxIgnoreQuotaInterval(interval uint64) {
	c.setUint64Config(hugeTxIgnoreQuotaIntervalKey, interval, "HugeTx ignore quota interval")
}

// SetDynamicBlockGasLimit sets the dynamic block gas limit for sequencer
func (c *ApolloConfigController) SetDynamicBlockGasLimit(gasLimit uint64) {
	c.setUint64Config(dynamicBlockGasLimitKey, gasLimit, "Dynamic block gas limit")
}

// SetHugeTxConfigWithGasLimit sets all four configuration parameters at once for hugetx e2e test
func (c *ApolloConfigController) SetHugeTxConfigWithGasLimit(thresholdRatio, quotaRatio, ignoreQuotaInterval, dynamicBlockGasLimit uint64) {
	c.t.Logf("Setting complete HugeTx configuration: threshold=%d%%, quota=%d%%, ignoreInterval=%d blocks, gasLimit=%d",
		thresholdRatio, quotaRatio, ignoreQuotaInterval, dynamicBlockGasLimit)

	// Clear and set pool namespace with all hugeTx parameters at once
	if err := c.clearNamespaceContent(apolloPoolNamespace); err != nil {
		c.t.Logf("Failed to clear pool namespace: %v", err)
		return
	}

	poolConfig := fmt.Sprintf("%s: %d\n%s: %d\n%s: %d\n",
		hugeTxThresholdRatioKey, thresholdRatio,
		hugeTxQuotaRatioKey, quotaRatio,
		hugeTxIgnoreQuotaIntervalKey, ignoreQuotaInterval)

	if err := c.updateNamespaceConfig(apolloPoolNamespace, poolConfig); err != nil {
		c.t.Logf("Failed to update pool config: %v", err)
		return
	}

	// Clear and set sequencer namespace
	if err := c.clearNamespaceContent(apolloSeqNamespace); err != nil {
		c.t.Logf("Failed to clear sequencer namespace: %v", err)
		return
	}

	seqConfig := fmt.Sprintf("%s: %d\n", dynamicBlockGasLimitKey, dynamicBlockGasLimit)
	if err := c.updateNamespaceConfig(apolloSeqNamespace, seqConfig); err != nil {
		c.t.Logf("Failed to update sequencer config: %v", err)
		return
	}

	c.t.Logf("Complete HugeTx configuration completed")
}

// setUint64Config is a helper method to set uint64 configuration values
func (c *ApolloConfigController) setUint64Config(key string, value uint64, description string) {
	valueStr := fmt.Sprintf("%d", value)
	c.setConfigKey(key, valueStr, description)
}

// Close cleans up the Apollo client and removes temporary files
func (c *ApolloConfigController) Close() {
	// Apollo client cleanup - no explicit cleanup required for agollo v4
	// The client will automatically handle cleanup when the application exits
	if c != nil {
		// Remove temporary cookie file
		if c.cookieFile != "" {
			_ = os.Remove(c.cookieFile)
		}
		c.t.Logf("Apollo config controller cleanup completed")
	}
}
