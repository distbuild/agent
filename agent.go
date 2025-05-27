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
		TCP      string `json:"TCP"`
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
		cmd := exec.Command(
			"docker",
			"run",
			"--network=host",
			"--rm",
			workerDocker,
		)
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

	cpu, err := getPhysicalCPUCount()
	if err != nil {
		log.Printf("Failed to get physical CPU count, falling back to logical: %v", err)
		cpu = runtime.NumCPU()
	}

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

// 获取本地IP地址，优先获取非虚拟网络接口的IP
func getLocalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	// 定义需要排除的网络接口名称前缀
	excludePrefixes := []string{
		"docker", "lo", "virbr", "veth", "vmnet", "tap", "tun",
	}

	var preferredIP string

	for _, iface := range ifaces {
		// 排除非活动接口
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		// 排除回环接口
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		// 排除虚拟网络接口
		if shouldExcludeInterface(iface.Name, excludePrefixes) {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			log.Printf("Error getting addresses for interface %s: %v", iface.Name, err)
			continue
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

			// 优先选择IPv4地址
			if ipv4 := ip.To4(); ipv4 != nil {
				return ipv4.String(), nil
			}

			// 保存IPv6地址作为备选
			if preferredIP == "" && ip.To16() != nil {
				preferredIP = ip.String()
			}
		}
	}

	// 如果没有找到IPv4地址，返回备选的IPv6地址
	if preferredIP != "" {
		return preferredIP, nil
	}

	return "", fmt.Errorf("no network interface found")
}

// 判断接口是否应该被排除
func shouldExcludeInterface(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// 获取物理CPU核数
func getPhysicalCPUCount() (int, error) {
	// 不同操作系统的处理方式
	switch runtime.GOOS {
	case "linux":
		// 尝试使用lscpu命令获取物理CPU核心数
		cmd := exec.Command("lscpu")
		output, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("failed to execute lscpu: %w", err)
		}

		lines := strings.Split(string(output), "\n")
		var physicalCores int

		for _, line := range lines {
			if strings.HasPrefix(line, "CPU(s):") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					cores, err := strconv.Atoi(fields[1])
					if err != nil {
						return 0, fmt.Errorf("failed to parse CPU count: %w", err)
					}
					physicalCores = cores
					break
				}
			}
		}

		if physicalCores > 0 {
			return physicalCores, nil
		}

		return 0, fmt.Errorf("failed to determine physical CPU count")

	case "windows":
		// Windows系统获取物理CPU核心数
		cmd := exec.Command("wmic", "cpu", "get", "NumberOfCores", "/format:list")
		output, err := cmd.Output()
		if err != nil {
			return 0, fmt.Errorf("failed to execute wmic: %w", err)
		}

		lines := strings.Split(string(output), "\n")
		var totalCores int

		for _, line := range lines {
			if strings.HasPrefix(line, "NumberOfCores=") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					cores, err := strconv.Atoi(strings.TrimSpace(parts[1]))
					if err != nil {
						return 0, fmt.Errorf("failed to parse CPU cores: %w", err)
					}
					totalCores += cores
				}
			}
		}

		if totalCores > 0 {
			return totalCores, nil
		}

		return 0, fmt.Errorf("failed to determine physical CPU count")

	default:
		// 其他系统返回逻辑CPU数量
		return runtime.NumCPU(), nil
	}
}

// 获取内存大小(GB)
func getMemoryGB() (int, error) {
	switch runtime.GOOS {
	case "linux":
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

	case "windows":
		cmd := exec.Command("wmic", "OS", "get", "TotalVisibleMemorySize", "/format:list")
		output, err := cmd.Output()
		if err != nil {
			return 0, err
		}

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "TotalVisibleMemorySize=") {
				parts := strings.SplitN(line, "=", 2)
				if len(parts) == 2 {
					kb, err := strconv.Atoi(strings.TrimSpace(parts[1]))
					if err != nil {
						return 0, err
					}
					return kb / (1024 * 1024), nil // 转换为 GB
				}
			}
		}

		return 0, fmt.Errorf("TotalVisibleMemorySize not found")

	default:
		return 0, fmt.Errorf("unsupported OS for memory detection")
	}
}

// 获取硬盘信息
func getDiskInfo() ([]DiskInfo, error) {
	var disks []DiskInfo

	switch runtime.GOOS {
	case "linux":
		cmd := exec.Command("lsblk", "-d", "-o", "NAME,SIZE", "-b", "--nodeps")
		output, err := cmd.Output()
		if err != nil {
			// 尝试使用fdisk命令作为备选
			cmd = exec.Command("fdisk", "-l")
			output, err = cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("failed to execute fdisk: %w", err)
			}

			lines := strings.Split(string(output), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "Disk /dev/") {
					// 解析行: "Disk /dev/sda: 1000.2 GB, 1000204886016 bytes, 1953525168 sectors"
					fields := strings.Fields(line)
					if len(fields) >= 4 {
						name := strings.TrimPrefix(fields[1], "/dev/")
						size := fields[3] + " " + fields[4]
						disks = append(disks, DiskInfo{
							Name: name,
							Size: size,
						})
					}
				}
			}

			if len(disks) > 0 {
				return disks, nil
			}

			return nil, fmt.Errorf("failed to get disk info")
		}

		lines := strings.Split(string(output), "\n")
		for i, line := range lines {
			if i == 0 || strings.TrimSpace(line) == "" {
				continue // 跳过标题行和空行
			}

			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}

			// 转换字节为人类可读格式
			sizeBytes, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				log.Printf("Failed to parse disk size: %v", err)
				continue
			}

			disks = append(disks, DiskInfo{
				Name: fields[0],
				Size: formatBytes(sizeBytes),
			})
		}

	case "windows":
		cmd := exec.Command("wmic", "diskdrive", "get", "Caption,Size", "/format:list")
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("failed to execute wmic: %w", err)
		}

		lines := strings.Split(string(output), "\n\n")
		for _, block := range lines {
			if strings.TrimSpace(block) == "" {
				continue
			}

			var name, size string
			fields := strings.Split(block, "\n")
			for _, field := range fields {
				if strings.HasPrefix(field, "Caption=") {
					name = strings.TrimPrefix(field, "Caption=")
				} else if strings.HasPrefix(field, "Size=") {
					sizeBytes := strings.TrimPrefix(field, "Size=")
					if sizeBytes != "" {
						sizeInt, err := strconv.ParseInt(sizeBytes, 10, 64)
						if err != nil {
							log.Printf("Failed to parse disk size: %v", err)
							continue
						}
						size = formatBytes(sizeInt)
					}
				}
			}

			if name != "" && size != "" {
				// 提取驱动器名称
				nameParts := strings.Fields(name)
				if len(nameParts) > 0 {
					name = nameParts[0]
				}

				disks = append(disks, DiskInfo{
					Name: name,
					Size: size,
				})
			}
		}

	default:
		return nil, fmt.Errorf("unsupported OS for disk detection")
	}

	if len(disks) == 0 {
		return nil, fmt.Errorf("no disks found")
	}

	return disks, nil
}

// 将字节转换为人类可读的格式
func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.2f TB", float64(bytes)/TB)
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
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
		Port:    22,
		Meta: map[string]string{
			"cpu":          strconv.Itoa(info.CPU),
			"memory":       strconv.Itoa(info.Memory),
			"disks":        string(disksJSON),
			"ip":           info.IP,
			"CreationTime": time.Now().UTC().Format(time.RFC3339), // 新增时间戳
		},
		Check: struct {
			TCP      string `json:"TCP"`
			Interval string `json:"Interval"`
		}{
			TCP:      fmt.Sprintf("%s:22", info.IP), // TCP检查22端口
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

	log.Println("Successfully registered to Consul with TCP check on port 22")

	return nil
}
