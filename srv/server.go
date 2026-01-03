package srv

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Hostname     string
	TemplatesDir string
	StaticDir    string
	UploadsDir   string
	AICache      *AIImageCache

	mu    sync.Mutex
	jobs  map[string]*Job
}

type Job struct {
	ID              string
	Status          string // "processing", "done", "error"
	Log             strings.Builder
	GCodePath       string
	OriginalName    string
	CreatedAt       time.Time
	MaxWidth        float64
	MaxHeight       float64
	ToolOn          string
	ToolOff         string
	UseAI           bool
	AIImageFilename string // Filename of AI-generated image in cache
	AIImageCached   bool   // Whether the AI image was served from cache
}

func New(hostname string) (*Server, error) {
	// Use DATA_DIR env var, or fall back to runtime.Caller for development
	baseDir := os.Getenv("DATA_DIR")
	if baseDir == "" {
		_, thisFile, _, _ := runtime.Caller(0)
		baseDir = filepath.Dir(thisFile)
		baseDir = filepath.Join(baseDir, "..") // Go up from srv/ to project root
	}
	uploadsDir := filepath.Join(baseDir, "uploads")
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		return nil, err
	}

	// Initialize AI image cache
	cacheDir := filepath.Join(baseDir, "ai_cache")
	dbPath := filepath.Join(baseDir, "ai_cache.db")
	aiCache, err := NewAIImageCache(dbPath, cacheDir)
	if err != nil {
		return nil, fmt.Errorf("init AI cache: %w", err)
	}

	templatesDir := os.Getenv("TEMPLATES_DIR")
	if templatesDir == "" {
		templatesDir = filepath.Join(baseDir, "srv", "templates")
	}
	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		staticDir = filepath.Join(baseDir, "srv", "static")
	}
	srv := &Server{
		Hostname:     hostname,
		TemplatesDir: templatesDir,
		StaticDir:    staticDir,
		UploadsDir:   uploadsDir,
		AICache:      aiCache,
		jobs:         make(map[string]*Job),
	}
	return srv, nil
}

func (s *Server) HandleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.renderTemplate(w, "index.html", map[string]interface{}{
		"Hostname": s.Hostname,
	}); err != nil {
		slog.Warn("render template", "url", r.URL.Path, "error", err)
	}
}

func (s *Server) HandleUpload(w http.ResponseWriter, r *http.Request) {
	// Max 50MB
	r.ParseMultipartForm(50 << 20)

	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Failed to read uploaded file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Parse dimension options
	maxWidth := 200.0
	maxHeight := 200.0
	if w := r.FormValue("maxWidth"); w != "" {
		if v, err := strconv.ParseFloat(w, 64); err == nil && v > 0 {
			maxWidth = v
		}
	}
	if h := r.FormValue("maxHeight"); h != "" {
		if v, err := strconv.ParseFloat(h, 64); err == nil && v > 0 {
			maxHeight = v
		}
	}

	// Parse tool control options
	toolOn := r.FormValue("toolOn")
	if toolOn == "" {
		toolOn = "S4 M0"
	}
	toolOff := r.FormValue("toolOff")
	if toolOff == "" {
		toolOff = "S4 M100"
	}

	// Parse AI transformation options
	useAI := r.FormValue("useAI") == "on" || r.FormValue("useAI") == "true"
	apiKey := r.FormValue("apiKey") // Never log this!
	aiPrompt := r.FormValue("aiPrompt")
	if aiPrompt == "" {
		aiPrompt = DefaultAIPrompt
	}

	// Generate job ID
	jobID := fmt.Sprintf("%d", time.Now().UnixNano())
	jobDir := filepath.Join(s.UploadsDir, jobID)
	if err := os.MkdirAll(jobDir, 0755); err != nil {
		http.Error(w, "Failed to create job directory: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save uploaded file
	ext := filepath.Ext(header.Filename)
	inputPath := filepath.Join(jobDir, "input"+ext)
	dst, err := os.Create(inputPath)
	if err != nil {
		http.Error(w, "Failed to save file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		http.Error(w, "Failed to save file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	dst.Close()

	// Create job
	job := &Job{
		ID:           jobID,
		Status:       "processing",
		OriginalName: header.Filename,
		CreatedAt:    time.Now(),
		MaxWidth:     maxWidth,
		MaxHeight:    maxHeight,
		ToolOn:       toolOn,
		ToolOff:      toolOff,
		UseAI:        useAI,
	}

	s.mu.Lock()
	s.jobs[jobID] = job
	s.mu.Unlock()

	// Process in background (pass apiKey and prompt directly, do not store)
	go s.processJob(job, jobDir, inputPath, apiKey, aiPrompt)

	// Redirect to job status page
	http.Redirect(w, r, "/job/"+jobID, http.StatusSeeOther)
}

func (s *Server) processJob(job *Job, jobDir, inputPath, apiKey, aiPrompt string) {
	svgPath := filepath.Join(jobDir, "output.svg")
	gcodePath := filepath.Join(jobDir, "output.gcode")

	// If AI transformation is enabled, run it first
	if job.UseAI {
		job.Log.WriteString("=== Running AI Image Transformation ===\n")

		// Hash the input image to check cache
		inputHash, err := HashFile(inputPath)
		if err != nil {
			job.Log.WriteString(fmt.Sprintf("Error hashing input file: %v\n", err))
			job.Status = "error"
			return
		}
		job.Log.WriteString(fmt.Sprintf("Input image hash: %s\n", inputHash[:16]))

		// Check cache first
		cached, err := s.AICache.Lookup(inputHash, aiPrompt)
		if err != nil {
			job.Log.WriteString(fmt.Sprintf("Cache lookup error: %v\n", err))
			// Continue with API call
		}

		var aiImagePath string
		if cached != nil {
			// Cache hit!
			job.Log.WriteString(fmt.Sprintf("Cache HIT - using cached result: %s\n", cached.Filename))
			aiImagePath = cached.FullPath
			job.AIImageFilename = cached.Filename
			job.AIImageCached = true
		} else {
			// Cache miss - call the API
			job.Log.WriteString("Cache MISS - calling Gemini API...\n")

			if apiKey == "" {
				job.Log.WriteString("Error: AI transformation enabled but no API key provided\n")
				job.Status = "error"
				return
			}

			imageData, mimeType, err := s.callGeminiAPI(inputPath, apiKey, aiPrompt)
			if err != nil {
				job.Log.WriteString(fmt.Sprintf("AI transformation error: %v\n", err))
				job.Status = "error"
				return
			}

			// Store in cache
			result, err := s.AICache.Store(inputHash, aiPrompt, imageData, mimeType)
			if err != nil {
				job.Log.WriteString(fmt.Sprintf("Warning: failed to cache result: %v\n", err))
				// Continue anyway - write to job dir instead
				ext := ".png"
				if mimeType == "image/jpeg" {
					ext = ".jpg"
				}
				aiImagePath = filepath.Join(jobDir, "ai_generated"+ext)
				if err := os.WriteFile(aiImagePath, imageData, 0644); err != nil {
					job.Log.WriteString(fmt.Sprintf("Error saving AI image: %v\n", err))
					job.Status = "error"
					return
				}
			} else {
				aiImagePath = result.FullPath
				job.AIImageFilename = result.Filename
			}
			job.Log.WriteString(fmt.Sprintf("AI transformation complete, saved as: %s\n", filepath.Base(aiImagePath)))
		}

		job.Log.WriteString("\n")

		// Use the AI-generated image as input for the rest of the pipeline
		inputPath = aiImagePath
	}

	// Run autotrace with centerline option
	job.Log.WriteString("=== Running autotrace ===\n")
	job.Log.WriteString(fmt.Sprintf("Command: autotrace -centerline -color-count 2 -output-file %s %s\n\n", svgPath, inputPath))

	cmd := exec.Command("autotrace", "-centerline", "-color-count", "2", "-output-file", svgPath, inputPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if stdout.Len() > 0 {
		job.Log.WriteString("stdout:\n")
		job.Log.WriteString(stdout.String())
		job.Log.WriteString("\n")
	}
	if stderr.Len() > 0 {
		job.Log.WriteString("stderr:\n")
		job.Log.WriteString(stderr.String())
		job.Log.WriteString("\n")
	}

	if err != nil {
		job.Log.WriteString(fmt.Sprintf("\nError: %v\n", err))
		job.Status = "error"
		return
	}
	job.Log.WriteString("autotrace completed successfully\n\n")

	// Remove white/near-white paths from SVG
	job.Log.WriteString("=== Filtering white paths from SVG ===\n")
	if err := filterWhitePaths(svgPath); err != nil {
		job.Log.WriteString(fmt.Sprintf("Warning: failed to filter white paths: %v\n", err))
	} else {
		job.Log.WriteString("White paths removed\n\n")
	}

	// Calculate DPI to achieve desired output size
	// svg2gcode uses DPI to convert pixels to mm: mm = pixels / DPI * 25.4
	// So to get desired mm from pixels: DPI = pixels / mm * 25.4
	svgWidth, svgHeight := getSVGDimensions(svgPath)
	job.Log.WriteString(fmt.Sprintf("SVG dimensions: %.2f x %.2f pixels\n", svgWidth, svgHeight))
	job.Log.WriteString(fmt.Sprintf("Max output dimensions: %.2f x %.2f mm\n", job.MaxWidth, job.MaxHeight))

	scaledWidth, scaledHeight := scaleToFit(svgWidth, svgHeight, job.MaxWidth, job.MaxHeight)
	job.Log.WriteString(fmt.Sprintf("Target output dimensions: %.2f x %.2f mm\n", scaledWidth, scaledHeight))

	// Calculate DPI: we need svgWidth pixels to equal scaledWidth mm
	// DPI = pixels per inch, and 1 inch = 25.4mm
	// So: scaledWidth = svgWidth / DPI * 25.4
	// Therefore: DPI = svgWidth / scaledWidth * 25.4
	dpi := svgWidth / scaledWidth * 25.4
	job.Log.WriteString(fmt.Sprintf("Calculated DPI: %.2f\n\n", dpi))

	dpiArg := fmt.Sprintf("%.4f", dpi)

	// Run svg2gcode
	job.Log.WriteString("=== Running svg2gcode ===\n")
	job.Log.WriteString(fmt.Sprintf("Command: svg2gcode --on '%s' --off '%s' --dpi %s %s -o %s\n\n", job.ToolOn, job.ToolOff, dpiArg, svgPath, gcodePath))

	cmd = exec.Command("svg2gcode", "--on", job.ToolOn, "--off", job.ToolOff, "--dpi", dpiArg, svgPath, "-o", gcodePath)
	stdout.Reset()
	stderr.Reset()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if stdout.Len() > 0 {
		job.Log.WriteString("stdout:\n")
		job.Log.WriteString(stdout.String())
		job.Log.WriteString("\n")
	}
	if stderr.Len() > 0 {
		job.Log.WriteString("stderr:\n")
		job.Log.WriteString(stderr.String())
		job.Log.WriteString("\n")
	}

	if err != nil {
		job.Log.WriteString(fmt.Sprintf("\nError: %v\n", err))
		job.Status = "error"
		return
	}
	job.Log.WriteString("svg2gcode completed successfully\n")

	job.GCodePath = gcodePath
	job.Status = "done"
}

func (s *Server) HandleJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	s.mu.Lock()
	job, exists := s.jobs[jobID]
	s.mu.Unlock()

	if !exists {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	jobDir := filepath.Join(s.UploadsDir, jobID)

	// Read SVG content if job is done
	var svgContent template.HTML
	if job.Status == "done" || job.Status == "error" {
		svgPath := filepath.Join(jobDir, "output.svg")
		if data, err := os.ReadFile(svgPath); err == nil {
			svgContent = template.HTML(data)
		}
	}

	// Build AI image URL if one exists
	var aiImageURL string
	if job.AIImageFilename != "" {
		aiImageURL = "/ai-cache/" + job.AIImageFilename
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.renderTemplate(w, "job.html", map[string]interface{}{
		"Job":        job,
		"Log":        job.Log.String(),
		"Hostname":   s.Hostname,
		"SVGContent": svgContent,
		"AIImageURL": aiImageURL,
	}); err != nil {
		slog.Warn("render template", "url", r.URL.Path, "error", err)
	}
}

func (s *Server) HandleDownload(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")

	s.mu.Lock()
	job, exists := s.jobs[jobID]
	s.mu.Unlock()

	if !exists || job.Status != "done" || job.GCodePath == "" {
		http.Error(w, "File not available", http.StatusNotFound)
		return
	}

	// Generate download filename from original
	baseName := strings.TrimSuffix(job.OriginalName, filepath.Ext(job.OriginalName))
	downloadName := baseName + ".gcode"

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, job.GCodePath)
}



func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) error {
	path := filepath.Join(s.TemplatesDir, name)
	tmpl, err := template.ParseFiles(path)
	if err != nil {
		return fmt.Errorf("parse template %q: %w", name, err)
	}
	if err := tmpl.Execute(w, data); err != nil {
		return fmt.Errorf("execute template %q: %w", name, err)
	}
	return nil
}

// getSVGDimensions extracts width and height from an SVG file
func getSVGDimensions(svgPath string) (width, height float64) {
	data, err := os.ReadFile(svgPath)
	if err != nil {
		return 100, 100 // default
	}

	// Try to match width and height attributes
	widthRe := regexp.MustCompile(`<svg[^>]*\swidth="([0-9.]+)`)
	heightRe := regexp.MustCompile(`<svg[^>]*\sheight="([0-9.]+)`)

	if m := widthRe.FindSubmatch(data); m != nil {
		width, _ = strconv.ParseFloat(string(m[1]), 64)
	}
	if m := heightRe.FindSubmatch(data); m != nil {
		height, _ = strconv.ParseFloat(string(m[1]), 64)
	}

	if width == 0 {
		width = 100
	}
	if height == 0 {
		height = 100
	}

	return width, height
}

// scaleToFit calculates dimensions that fit within maxW x maxH while maintaining aspect ratio
func scaleToFit(srcW, srcH, maxW, maxH float64) (float64, float64) {
	if srcW <= 0 || srcH <= 0 {
		return maxW, maxH
	}

	// Calculate scale factors
	scaleW := maxW / srcW
	scaleH := maxH / srcH

	// Use the smaller scale to fit within bounds
	scale := scaleW
	if scaleH < scaleW {
		scale = scaleH
	}

	return srcW * scale, srcH * scale
}

// filterWhitePaths removes paths with white or near-white stroke colors from an SVG file
func filterWhitePaths(svgPath string) error {
	data, err := os.ReadFile(svgPath)
	if err != nil {
		return err
	}

	// Match path elements with stroke color
	pathRegex := regexp.MustCompile(`<path[^>]*style="[^"]*stroke:#([0-9a-fA-F]{6})[^"]*"[^>]*/>`)

	filtered := pathRegex.ReplaceAllFunc(data, func(match []byte) []byte {
		// Extract the color
		colorMatch := regexp.MustCompile(`stroke:#([0-9a-fA-F]{6})`).FindSubmatch(match)
		if colorMatch == nil {
			return match
		}

		hexColor := string(colorMatch[1])
		if isNearWhite(hexColor) {
			return []byte{} // Remove the path
		}
		return match
	})

	return os.WriteFile(svgPath, filtered, 0644)
}

// isNearWhite checks if a hex color is white or near-white (high RGB values)
func isNearWhite(hex string) bool {
	if len(hex) != 6 {
		return false
	}

	r, _ := strconv.ParseInt(hex[0:2], 16, 64)
	g, _ := strconv.ParseInt(hex[2:4], 16, 64)
	b, _ := strconv.ParseInt(hex[4:6], 16, 64)

	// Consider colors with all components > 240 as "near white"
	threshold := int64(240)
	return r > threshold && g > threshold && b > threshold
}

// callGeminiAPI calls the Gemini API to transform an image to line art
// Returns the raw image data and mime type
func (s *Server) callGeminiAPI(inputPath, apiKey, prompt string) (imageData []byte, mimeType string, err error) {
	// Read the input image
	inputData, err := os.ReadFile(inputPath)
	if err != nil {
		return nil, "", fmt.Errorf("read input image: %w", err)
	}

	// Determine MIME type from extension
	ext := strings.ToLower(filepath.Ext(inputPath))
	inputMimeType := mime.TypeByExtension(ext)
	if inputMimeType == "" {
		// Fallback for common types
		switch ext {
		case ".png":
			inputMimeType = "image/png"
		case ".jpg", ".jpeg":
			inputMimeType = "image/jpeg"
		case ".webp":
			inputMimeType = "image/webp"
		case ".gif":
			inputMimeType = "image/gif"
		case ".bmp":
			inputMimeType = "image/bmp"
		default:
			inputMimeType = "image/png"
		}
	}

	// Base64 encode the image
	imageBase64 := base64.StdEncoding.EncodeToString(inputData)

	// Build the API request (prompt is passed in as parameter)

	reqBody := map[string]interface{}{
		"contents": []map[string]interface{}{
			{
				"parts": []map[string]interface{}{
					{"text": prompt},
					{
						"inline_data": map[string]string{
							"mime_type": inputMimeType,
							"data":      imageBase64,
						},
					},
				},
			},
		},
		"generationConfig": map[string]interface{}{
			"responseModalities": []string{"text", "image"},
		},
	}

	reqJSON, err := json.Marshal(reqBody)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request: %w", err)
	}

	// Call the Gemini API
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash-exp:generateContent?key=%s", apiKey)
	req, err := http.NewRequest("POST", url, bytes.NewReader(reqJSON))
	if err != nil {
		return nil, "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to extract error message, but be careful not to leak API key
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error.Message != "" {
			return nil, "", fmt.Errorf("API error: %s", errResp.Error.Message)
		}
		return nil, "", fmt.Errorf("API error (status %d)", resp.StatusCode)
	}

	// Parse the response
	var apiResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text       string `json:"text,omitempty"`
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData,omitempty"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, "", fmt.Errorf("parse response: %w", err)
	}

	// Find the image in the response
	for _, candidate := range apiResp.Candidates {
		for _, part := range candidate.Content.Parts {
			if part.InlineData != nil {
				// Decode the image
				imgData, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return nil, "", fmt.Errorf("decode image: %w", err)
				}
				return imgData, part.InlineData.MimeType, nil
			}
		}
	}

	return nil, "", fmt.Errorf("no image in API response")
}

// Serve starts the HTTP server with the configured routes
func (s *Server) Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.HandleRoot)
	mux.HandleFunc("POST /upload", s.HandleUpload)
	mux.HandleFunc("GET /job/{id}", s.HandleJobStatus)
	mux.HandleFunc("GET /download/{id}", s.HandleDownload)

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.StaticDir))))
	mux.Handle("/ai-cache/", http.StripPrefix("/ai-cache/", http.FileServer(http.Dir(s.AICache.CacheDir()))))
	slog.Info("starting server", "addr", addr)
	return http.ListenAndServe(addr, mux)
}
