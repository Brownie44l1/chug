package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type Config struct {
	ServerURL   string
	APIKey      string
	Concurrency int
	TotalJobs   int
	DupPercent  int
}

type JobResult struct {
	JobID      string
	SubmitTime time.Time
	APILatency time.Duration
	Err        error
	Status     string
	StorageURL string
	FinishTime time.Time
}

func main() {
	cfg := parseFlags()

	fmt.Println("=======================================================")
	fmt.Println("             CHUG SERVICE BENCHMARK & LOAD TEST        ")
	fmt.Println("=======================================================")
	fmt.Printf("Server URL:        %s\n", cfg.ServerURL)
	fmt.Printf("Total Uploads:     %d\n", cfg.TotalJobs)
	fmt.Printf("Concurrency:       %d workers\n", cfg.Concurrency)
	fmt.Printf("Duplicates Rate:   %d%%\n", cfg.DupPercent)
	fmt.Println("=======================================================")

	// 1. Health Check
	fmt.Print("Checking service health... ")
	if err := checkHealth(cfg.ServerURL); err != nil {
		fmt.Printf("\n[ERROR] Service is not healthy: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("OK")

	// 2. Generate Payloads
	fmt.Print("Preparing image payloads... ")
	payloads, checksums := generatePayloads(cfg.TotalJobs, cfg.DupPercent)
	fmt.Printf("Done (%d unique, %d duplicates)\n", len(payloads)-countDuplicates(checksums), countDuplicates(checksums))

	// 3. Start Benchmarking Uploads
	fmt.Println("\nSubmitting upload jobs...")
	results := make([]*JobResult, cfg.TotalJobs)
	for i := 0; i < cfg.TotalJobs; i++ {
		results[i] = &JobResult{}
	}

	jobChan := make(chan int, cfg.TotalJobs)
	for i := 0; i < cfg.TotalJobs; i++ {
		jobChan <- i
	}
	close(jobChan)

	var wg sync.WaitGroup
	startTime := time.Now()

	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 10 * time.Second}
			for i := range jobChan {
				res := results[i]
				res.SubmitTime = time.Now()

				jobID, latency, err := submitUpload(client, cfg.ServerURL, cfg.APIKey, payloads[i], checksums[i])
				res.JobID = jobID
				res.APILatency = latency
				res.Err = err
			}
		}()
	}

	wg.Wait()
	submitDuration := time.Since(startTime)
	fmt.Printf("Finished submitting all jobs in %v\n", submitDuration.Round(time.Millisecond))

	// 4. Poll Job Completion Statuses
	fmt.Println("\nWaiting for background workers to complete processing...")
	pollWorkers(cfg.ServerURL, cfg.APIKey, results)

	// 5. Calculate Metrics
	printReport(results, submitDuration)
}

func parseFlags() Config {
	serverURL := flag.String("server", "http://localhost:8080", "Base URL of the Chug service")
	apiKey := flag.String("key", "", "API key for authentication (optional, defaults to env API_KEY)")
	concurrency := flag.Int("concurrency", 10, "Number of concurrent uploaders")
	total := flag.Int("total", 100, "Total number of upload jobs to submit")
	dups := flag.Int("dups", 30, "Percentage of duplicate uploads (0-100)")

	flag.Parse()

	keyVal := *apiKey
	if keyVal == "" {
		keyVal = os.Getenv("API_KEY")
		if keyVal == "" {
			keyVal = "chug_demo_developer_key" // Fallback default demo key
		}
	}

	return Config{
		ServerURL:   *serverURL,
		APIKey:      keyVal,
		Concurrency: *concurrency,
		TotalJobs:   *total,
		DupPercent:  *dups,
	}
}

func checkHealth(serverURL string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(serverURL + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status code %d", resp.StatusCode)
	}
	return nil
}

func generateDummyPNG(seed int) []byte {
	// Minimal valid image/png magic prefix + dummy content
	data := make([]byte, 128)
	data[0] = 0x89
	data[1] = 0x50
	data[2] = 0x4E
	data[3] = 0x47
	// Seed to make it unique
	binary.BigEndian.PutUint32(data[4:8], uint32(seed))
	return data
}

func generatePayloads(total int, dupRate int) ([][]byte, []string) {
	payloads := make([][]byte, total)
	checksums := make([]string, total)

	uniqueCount := total - (total * dupRate / 100)
	if uniqueCount < 1 {
		uniqueCount = 1
	}

	uniquePayloads := make([][]byte, uniqueCount)
	uniqueChecksums := make([]string, uniqueCount)

	for i := 0; i < uniqueCount; i++ {
		b := generateDummyPNG(i + 1)
		uniquePayloads[i] = b
		h := sha256.New()
		h.Write(b)
		uniqueChecksums[i] = hex.EncodeToString(h.Sum(nil))
	}

	for i := 0; i < total; i++ {
		// Distribute unique payloads or repeat them
		idx := i % uniqueCount
		payloads[i] = uniquePayloads[idx]
		checksums[i] = uniqueChecksums[idx]
	}

	return payloads, checksums
}

func countDuplicates(checksums []string) int {
	seen := make(map[string]bool)
	dups := 0
	for _, c := range checksums {
		if seen[c] {
			dups++
		} else {
			seen[c] = true
		}
	}
	return dups
}

func submitUpload(client *http.Client, serverURL, apiKey string, fileBytes []byte, checksum string) (string, time.Duration, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", "benchmark.png")
	if err != nil {
		return "", 0, err
	}
	if _, err := part.Write(fileBytes); err != nil {
		return "", 0, err
	}

	if err := writer.WriteField("checksum", checksum); err != nil {
		return "", 0, err
	}
	_ = writer.Close()

	req, err := http.NewRequest("POST", serverURL+"/uploads", body)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+apiKey)

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		return "", latency, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(resp.Body)
		return "", latency, fmt.Errorf("HTTP status %d: %s", resp.StatusCode, string(b))
	}

	var resObj struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resObj); err != nil {
		return "", latency, err
	}

	return resObj.JobID, latency, nil
}

func pollWorkers(serverURL, apiKey string, results []*JobResult) {
	client := &http.Client{Timeout: 5 * time.Second}
	completed := 0
	total := len(results)

	for completed < total {
		completed = 0
		pendingList := 0

		for _, res := range results {
			if res.Err != nil {
				completed++
				continue
			}
			if res.Status == "success" || res.Status == "failed" {
				completed++
				continue
			}

			// Poll status
			req, err := http.NewRequest("GET", serverURL+"/uploads/"+res.JobID, nil)
			if err != nil {
				continue
			}
			req.Header.Set("Authorization", "Bearer "+apiKey)

			resp, err := client.Do(req)
			if err != nil {
				continue
			}

			if resp.StatusCode == http.StatusOK {
				var statusObj struct {
					Status     string `json:"status"`
					StorageURL string `json:"storage_url"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&statusObj); err == nil {
					if statusObj.Status == "success" || statusObj.Status == "failed" {
						res.Status = statusObj.Status
						res.StorageURL = statusObj.StorageURL
						res.FinishTime = time.Now()
					} else {
						pendingList++
					}
				}
			}
			resp.Body.Close()
		}

		// Print simple progress
		progress := float64(completed) / float64(total) * 100
		fmt.Printf("\rProcessing Progress: %.1f%% (%d/%d jobs finished, %d pending)   ", progress, completed, total, pendingList)
		if completed < total {
			time.Sleep(500 * time.Millisecond)
		}
	}
	fmt.Println()
}

func printReport(results []*JobResult, submitDuration time.Duration) {
	var apiLatencies []time.Duration
	var turnaroundTimes []time.Duration
	successCount := 0
	failedCount := 0
	submitFailedCount := 0

	storageURLs := make(map[string]bool)

	for _, res := range results {
		if res.Err != nil {
			submitFailedCount++
			continue
		}

		apiLatencies = append(apiLatencies, res.APILatency)

		if res.Status == "success" {
			successCount++
			turnaround := res.FinishTime.Sub(res.SubmitTime)
			turnaroundTimes = append(turnaroundTimes, turnaround)
			if res.StorageURL != "" {
				storageURLs[res.StorageURL] = true
			}
		} else if res.Status == "failed" {
			failedCount++
		}
	}

	sort.Slice(apiLatencies, func(i, j int) bool { return apiLatencies[i] < apiLatencies[j] })
	sort.Slice(turnaroundTimes, func(i, j int) bool { return turnaroundTimes[i] < turnaroundTimes[j] })

	totalCompleted := len(apiLatencies)

	fmt.Println("\n================ CHUG PERFORMANCE REPORT ================")
	fmt.Printf("Total Uploads Attempted:   %d\n", len(results))
	fmt.Printf("Failed Submissions:        %d\n", submitFailedCount)
	fmt.Printf("Worker Success Count:      %d\n", successCount)
	fmt.Printf("Worker Failure Count:      %d\n", failedCount)
	fmt.Printf("Submissions Throughput:    %.2f req/sec\n", float64(len(results))/submitDuration.Seconds())

	if totalCompleted > 0 {
		avgAPI := averageDuration(apiLatencies)
		p50API := percentileDuration(apiLatencies, 0.50)
		p90API := percentileDuration(apiLatencies, 0.90)
		p99API := percentileDuration(apiLatencies, 0.99)

		fmt.Println("\n--- API Latency (Response Time for /uploads request) ---")
		fmt.Printf("  Average:                 %v\n", avgAPI.Round(time.Microsecond))
		fmt.Printf("  P50 (Median):            %v\n", p50API.Round(time.Microsecond))
		fmt.Printf("  P90:                     %v\n", p90API.Round(time.Microsecond))
		fmt.Printf("  P99:                     %v\n", p99API.Round(time.Microsecond))
	}

	if len(turnaroundTimes) > 0 {
		avgTurnaround := averageDuration(turnaroundTimes)
		p50Turn := percentileDuration(turnaroundTimes, 0.50)
		p90Turn := percentileDuration(turnaroundTimes, 0.90)
		p99Turn := percentileDuration(turnaroundTimes, 0.99)

		fmt.Println("\n--- Background Worker Processing Turnaround Time ---")
		fmt.Printf("  Average:                 %v\n", avgTurnaround.Round(time.Millisecond))
		fmt.Printf("  P50 (Median):            %v\n", p50Turn.Round(time.Millisecond))
		fmt.Printf("  P90:                     %v\n", p90Turn.Round(time.Millisecond))
		fmt.Printf("  P99:                     %v\n", p99Turn.Round(time.Millisecond))

		// Idempotency Calculations
		uniqueURLs := len(storageURLs)
		idempotencyHits := successCount - uniqueURLs
		bandwidthSavedKB := float64(idempotencyHits*128) / 1024.0

		fmt.Println("\n--- Optimization & Storage Efficiency ---")
		fmt.Printf("  Unique Image Files:      %d\n", uniqueURLs)
		fmt.Printf("  Idempotency Cache Hits:  %d (bypassed R2 storage upload)\n", idempotencyHits)
		fmt.Printf("  Bandwidth & Storage Saved: %.2f KB\n", bandwidthSavedKB)
	}
	fmt.Println("=========================================================")
}

func averageDuration(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	var total time.Duration
	for _, d := range durs {
		total += d
	}
	return total / time.Duration(len(durs))
}

func percentileDuration(durs []time.Duration, pct float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	idx := int(float64(len(durs)-1) * pct)
	return durs[idx]
}
