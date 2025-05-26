package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

//go:embed .env
var envFile string

var (
	BuildTime string
	CommitID  string
)

type ServerInfo struct {
	IP       string     `json:"ip"`
	Hostname string     `json:"hostname"`
	CPU      int        `json:"cpu"`
	Memory   int        `json:"memory"` // in GB
	Disks    []DiskInfo `json:"disks"`
}

type DiskInfo struct {
	Name string `json:"name"`
	Size string `json:"size"`
}

type ConsulService struct {
	Name    string            `json:"Name"`
	Address string            `json:"Address"`
	Port    int               `json:"Port"`
	Meta    map[string]string `json:"Meta"`
	Check   struct {
		HTTP     string `json:"HTTP"`
		Interval string `json:"Interval"`
	} `json:"Check"`
}

var rootCmd = &cobra.Command{
	Use:     "agent",
	Short:   "boong agent",
	Version: BuildTime + "-" + CommitID,
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		if err := run(ctx); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
	},
}

// nolint:gochecknoinits
func init() {
	cobra.OnInitialize()

	rootCmd.Root().CompletionOptions.DisableDefaultCmd = true
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	if err := loadEnvFile(envFile); err != nil {
		return fmt.Errorf("load .env failed: %w", err)
	}

	// 初始注册
	info, err := collectServerInfo()
	if err != nil {
		return fmt.Errorf("failed to collect server info: %v", err)
	}

	// 启动HTTP服务器
	go func() {
		log.Println("Starting HTTP server on :8080")
		http.HandleFunc("/info", infoHandler(info))
		http.HandleFunc("/health", healthHandler)
		if err := http.ListenAndServe(":8080", nil); err != nil {
			log.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// 创建定时器（每小时执行）
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	// 定时注册任务
	go func() {
		for {
			select {
			case <-ticker.C:
				newInfo, err := collectServerInfo()
				if err != nil {
					log.Printf("Failed to collect server info: %v", err)
					continue
				}

				if err := registerToConsul(newInfo); err != nil {
					log.Printf("Consul re-registration failed: %v", err)
				} else {
					log.Println("Successfully re-registered to Consul")
					// 更新内存中的info数据
					info = newInfo
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// 初始注册（移动到定时器之后，确保首次注册使用最新数据）
	if err := registerToConsul(info); err != nil {
		return fmt.Errorf("initial consul registration failed: %v", err)
	}

	workerDocker, exists := os.LookupEnv("WORKER_DOCKER")
	if !exists {
		return fmt.Errorf("environment variable WORKER_DOCKER not set")
	}

	// 运行Docker容器
	go func() {
		log.Println("Starting Docker container")
		cmd := exec.Command("docker", "run", "--rm", workerDocker)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			log.Printf("Docker run failed: %v", err)
		}
	}()

	// 保持主程序运行
	select {}
}

func loadEnvFile(content string) error {
	scanner := bufio.NewScanner(strings.NewReader(content))

	for scanner.Scan() {
		line := scanner.Text()
		// Skip comments or empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		_ = os.Setenv(key, value)
	}

	return nil
}

func collectServerInfo() (*ServerInfo, error) {
	ip, err := getLocalIP()
	if err != nil {
		return nil, fmt.Errorf("failed to get IP: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to get hostname: %w", err)
	}

	cpu := runtime.NumCPU()

	memory, err := getMemoryGB()
	if err != nil {
		return nil, fmt.Errorf("failed to get memory: %w", err)
	}

	disks, err := getDiskInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get disk info: %w", err)
	}

	return &ServerInfo{
		IP:       ip,
		Hostname: hostname,
		CPU:      cpu,
		Memory:   memory,
		Disks:    disks,
	}, nil
}

func getLocalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ipv4 := ip.To4(); ipv4 != nil {
				return ipv4.String(), nil
			}
		}
	}

	return "", fmt.Errorf("no network interface found")
}

func getMemoryGB() (int, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		if strings.HasPrefix(line, "MemTotal:") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				return 0, fmt.Errorf("invalid MemTotal line")
			}

			kb, err := strconv.Atoi(parts[1])
			if err != nil {
				return 0, err
			}

			return kb / (1024 * 1024), nil // 转换为 GB
		}
	}

	return 0, fmt.Errorf("MemTotal not found")
}

func getDiskInfo() ([]DiskInfo, error) {
	cmd := exec.Command("lsblk", "-d", "-o", "NAME,SIZE")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var disks []DiskInfo
	lines := strings.Split(string(output), "\n")

	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue // 跳过标题行和空行
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		disks = append(disks, DiskInfo{
			Name: fields[0],
			Size: fields[1],
		})
	}

	return disks, nil
}

func infoHandler(info *ServerInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(info); err != nil {
			http.Error(w, "failed to encode response", http.StatusInternalServerError)
		}
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func registerToConsul(info *ServerInfo) error {
	disksJSON, err := json.Marshal(info.Disks)
	if err != nil {
		return fmt.Errorf("failed to marshal disks info: %v", err)
	}

	service := ConsulService{
		Name:    info.Hostname,
		Address: info.IP,
		Port:    8080,
		Meta: map[string]string{
			"cpu":          strconv.Itoa(info.CPU),
			"memory":       strconv.Itoa(info.Memory),
			"disks":        string(disksJSON),
			"ip":           info.IP,
			"CreationTime": time.Now().UTC().Format(time.RFC3339), // 新增时间戳
		},
		Check: struct {
			HTTP     string `json:"HTTP"`
			Interval string `json:"Interval"`
		}{
			HTTP:     fmt.Sprintf("http://%s:8080/health", info.IP),
			Interval: "10s",
		},
	}

	jsonData, err := json.Marshal(service)
	if err != nil {
		return err
	}

	consulService, exists := os.LookupEnv("CONSUL_SERVICE")
	if !exists {
		return fmt.Errorf("environment variable CONSUL_SERVICE not set")
	}

	req, err := http.NewRequest(
		"PUT",
		consulService,
		bytes.NewBuffer(jsonData),
	)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("consul registration failed: %s", string(body))
	}

	log.Println("Successfully registered to Consul with metadata")

	return nil
}
