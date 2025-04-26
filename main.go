package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fatih/color"
	_ "github.com/mattn/go-sqlite3" // Import SQLite driver
	"github.com/ollama/ollama/api"
	"github.com/philippgille/chromem-go" // Chromem in-memory vector DB
	"github.com/schollz/progressbar/v3"
)

// Config holds the global configuration values
type Config struct {
	DocsDir            string `json:"docs_dir"`
	EmbeddingModel     string `json:"embedding_model"`
	CodeChatModel      string `json:"code_chat_model"`
	DocumentationModel string `json:"documentation_model"`
	OllamaHost         string `json:"ollama_host"`
	HashDBPath         string `json:"hash_db_path"`   // Path to chromem DB directory
	SQLiteDBPath       string `json:"sqlite_db_path"` // Path to the SQLite database
	WebPort            string `json:"web_port"`       // Port for the web UI
	GitBinPath         string `json:"git_bin_path"`   // Path to git binary
}

// DefaultConfig returns the default global configuration
func DefaultConfig() Config {
	return Config{
		DocsDir:            "./docs",
		EmbeddingModel:     "nomic-embed-text",
		CodeChatModel:      "qwen2.5-coder:1.5b",
		DocumentationModel: "llama3.2:1b",
		OllamaHost:         "http://localhost:11434",
		HashDBPath:         "./db",           // Default vector DB path
		SQLiteDBPath:       "file_hashes.db", // Default SQLite database path
		WebPort:            "8080",           // Default web port
	}
}

// LoadConfig loads the global configuration from a file or creates it with default values
func LoadConfig(filename string) (Config, error) {
	var config Config

	// Check if the config file exists
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		// Create the config file with default values
		config = DefaultConfig()
		configJSON, err := json.MarshalIndent(config, "", "  ")
		if err != nil {
			return config, fmt.Errorf("failed to marshal default config: %v", err)
		}

		// Write the default config to the file
		if err := ioutil.WriteFile(filename, configJSON, 0644); err != nil {
			return config, fmt.Errorf("failed to write config file: %v", err)
		}

		fmt.Printf("Default config created at %s — continuing with defaults", filename)
		// carry on with the default config
		return config, nil
	}

	// Load the config file
	configJSON, err := ioutil.ReadFile(filename)
	if err != nil {
		return config, fmt.Errorf("failed to read config file: %v", err)
	}

	// Parse the config file
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return config, fmt.Errorf("failed to parse config file: %v", err)
	}

	return config, nil
}

// ProjectConfig holds the configuration for a specific project
type ProjectConfig struct {
	ProjectName       string    `json:"project_name"`
	ProjectPath       string    `json:"project_path"`
	ExcludeFolders    []string  `json:"exclude_folders"`
	ExcludeFiles      []string  `json:"exclude_files"` // (optional)
	LastUpdated       time.Time `json:"last_updated"`
	TotalIndexedFiles int       `json:"total_indexed_files"`
	TotalFailedFiles  int       `json:"total_failed_files"`
}

type CodeAssistant struct {
	vectorDB      *chromem.DB   // Chromem in-memory vector DB
	config        Config        // Global configuration values
	db            *sql.DB       // SQLite database connection
	projectConfig ProjectConfig // Project-specific config
	projects      []string      // List of indexed projects
}

func MakeModelsAvailable(config Config) error {
	models := []string{
		config.EmbeddingModel,
		config.CodeChatModel,
		config.DocumentationModel,
	}

	for _, model := range models {
		// Use Ollama CLI or API to download the model if missing
		cmd := exec.Command("ollama", "pull", model)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		fmt.Printf("Ensuring model is available: %s\n", model)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to pull model %s: %v", model, err)
		}
	}

	return nil
}

func NewCodeAssistant(config Config) *CodeAssistant {
	// Initialize Chromem in-memory vector DB
	dbChromem, err := chromem.NewPersistentDB(config.HashDBPath, false)
	if err != nil {
		fmt.Printf("failed to create chromem client: %v\n", err)
		return nil
	}

	// Initialize SQLite database
	db, err := sql.Open("sqlite3", config.SQLiteDBPath)
	if err != nil {
		log.Fatalf("Failed to open SQLite database: %v", err)
		return nil // Or handle the error as appropriate
	}

	// Create the file_hashes table if it doesn't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS file_hashes (
			file_path TEXT PRIMARY KEY,
			hash TEXT
		)
	`)
	if err != nil {
		log.Fatalf("Failed to create file_hashes table: %v", err)
		return nil // Or handle the error as appropriate
	}

	return &CodeAssistant{
		vectorDB: dbChromem,
		config:   config,
		db:       db,
	}
}

// calculateMD5Hash calculates the MD5 hash of a file.
func calculateMD5Hash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hash := md5.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// getFileHash retrieves the file hash from the database.
func (ca *CodeAssistant) getFileHash(filePath string) (string, bool, error) {
	var hash string
	err := ca.db.QueryRow("SELECT hash FROM file_hashes WHERE file_path = ?", filePath).Scan(&hash)
	if err == sql.ErrNoRows {
		return "", false, nil // File not found in the database
	} else if err != nil {
		return "", false, err // Other error
	}
	return hash, true, nil // Hash found
}

// setFileHash stores the file hash in the database.
func (ca *CodeAssistant) setFileHash(filePath, hash string) error {
	_, err := ca.db.Exec("INSERT OR REPLACE INTO file_hashes (file_path, hash) VALUES (?, ?)", filePath, hash)
	return err
}

func (ca *CodeAssistant) parseDirectory(directoryPath string, excludeDirs []string, excludeFiles []string) ([]string, error) {
	var files []string
	err := filepath.Walk(directoryPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			for _, exclude := range excludeDirs {
				if strings.Contains(path, exclude) {
					return filepath.SkipDir
				}
			}
		} else {
			// Exclude specific files
			for _, excludeFile := range excludeFiles {
				if filepath.Base(path) == excludeFile {
					return nil // Skip the file
				}
			}

			ext := filepath.Ext(path)
			if ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".java" || ext == ".cpp" || ext == ".c" || ext == ".go" || ext == ".vue" || ext == ".jsx" || ext == ".tsx" {
				files = append(files, path)
			}
		}
		return nil
	})
	return files, err
}

func (ca *CodeAssistant) generateComments(code string) (string, error) {
	// Initialize the Ollama client
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return "", fmt.Errorf("failed to create Ollama client: %v", err)
	}

	// Prepare the prompt
	prompt := fmt.Sprintf(`%s
		Generate comments and documentation for this piece of code, only return text and do not return any code.

		Do not skip any function defined. It is critically important that we cover all functions.
		Also generate documentation only for functions and classes which are defined.

		Documentation should be at function level or class level, no line-specific comments should be returned.`, code)

	// Create the chat request
	req := &api.ChatRequest{
		Model: ca.config.DocumentationModel,
		Messages: []api.Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
	}

	var responseContent strings.Builder
	respFunc := func(resp api.ChatResponse) error {
		responseContent.WriteString(resp.Message.Content)
		return nil
	}

	// Send the request to Ollama
	err = client.Chat(context.Background(), req, respFunc)
	if err != nil {
		return "", fmt.Errorf("failed to generate comments: %v", err)
	}

	// Return the generated comments
	return responseContent.String(), nil
}

// loadProjectConfig loads the project config from a JSON file.
func (ca *CodeAssistant) loadProjectConfig(projectName string) (ProjectConfig, error) {
	configPath := filepath.Join(ca.config.DocsDir, projectName, "project_config.json")
	var config ProjectConfig
	configFile, err := os.Open(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return an empty config if the file doesn't exist
			return ProjectConfig{}, nil
		}
		return ProjectConfig{}, err
	}
	defer configFile.Close()

	byteValue, _ := ioutil.ReadFile(configPath)

	err = json.Unmarshal(byteValue, &config)
	if err != nil {
		return ProjectConfig{}, err
	}
	return config, nil
}

// saveProjectConfig saves the project config to a JSON file.
func (ca *CodeAssistant) saveProjectConfig(config ProjectConfig) error {
	configPath := filepath.Join(ca.config.DocsDir, config.ProjectName, "project_config.json")

	// Ensure the directory exists
	err := os.MkdirAll(filepath.Dir(configPath), os.ModePerm)
	if err != nil {
		return err
	}

	// Create the file
	configFile, err := os.Create(configPath) // Use os.Create instead of json.Create
	if err != nil {
		return err
	}
	defer configFile.Close()

	// Create a JSON encoder
	encoder := json.NewEncoder(configFile)
	encoder.SetIndent("", "  ") // Optional: Add indentation for readability

	// Encode the config to the file
	err = encoder.Encode(config)
	return err
}
func (ca *CodeAssistant) getProjectDetails() (string, string, []string, []string, error) {

	var projectName string
	fmt.Print("Enter project name: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	projectName = scanner.Text()

	fmt.Print("Enter codebase path: ")
	scanner = bufio.NewScanner(os.Stdin)
	scanner.Scan()
	path := scanner.Text()

	fmt.Print("Folders to exclude (comma separated): ")
	scanner = bufio.NewScanner(os.Stdin)
	scanner.Scan()
	exclude := []string{}
	temp := scanner.Text()
	if len(temp) > 0 {
		exclude = strings.Split(temp, ",")
	}
	for i := range exclude {
		exclude[i] = strings.TrimSpace(exclude[i])
	}
	fmt.Print("Files to exclude (comma separated): ")
	scanner = bufio.NewScanner(os.Stdin)
	scanner.Scan()
	excludeFiles := []string{}
	tempFiles := scanner.Text()
	if len(tempFiles) > 0 {
		excludeFiles = strings.Split(tempFiles, ",")
	}
	for i := range excludeFiles {
		excludeFiles[i] = strings.TrimSpace(excludeFiles[i])
	}

	return projectName, path, exclude, excludeFiles, nil
}

func (ca *CodeAssistant) indexCodebase(reindexProject string) error {
	var projectName, path string
	var exclude, excludeFiles []string
	var err error

	if reindexProject != "" {
		ca.projectConfig, err = ca.loadProjectConfig(reindexProject)

		if err != nil {
			fmt.Printf("Error loading project config: %v\n", err)
			// Optionally, prompt the user for details if the config is missing/invalid
			projectName, path, exclude, excludeFiles, err = ca.getProjectDetails()
			if err != nil {
				return fmt.Errorf("error getting project details: %v", err)
			}
		} else {
			projectName = ca.projectConfig.ProjectName
			path = ca.projectConfig.ProjectPath
			exclude = ca.projectConfig.ExcludeFolders
			excludeFiles = ca.projectConfig.ExcludeFiles
		}

	} else {

		projectName, path, exclude, excludeFiles, err = ca.getProjectDetails()
		if err != nil {
			return fmt.Errorf("error getting project details: %v", err)
		}
	}
	defaultExcludes := []string{"/node_modules", "/venv", "/build", "/dist", "/.venv", "/log", "/node_modules/", "/venv/", "/build/", "/dist/", "/.venv/", "/log/", "/.vite/", "/.git/"}

	exclude = append(exclude, defaultExcludes...)

	projectDocsDir := filepath.Join(ca.config.DocsDir, projectName)
	files, err := ca.parseDirectory(path, exclude, excludeFiles)
	if err != nil {
		return err
	}

	fmt.Printf("Indexing %d files...\n", len(files))
	processedFiles := 0
	failedFiles := 0
	updatedFiles := 0 // Track the number of files that need reindexing

	ca.projectConfig, err = ca.loadProjectConfig(reindexProject)
	// Save the project config
	if err != nil {
		ca.projectConfig = ProjectConfig{
			ProjectName:       projectName,
			ProjectPath:       path,
			ExcludeFolders:    exclude,
			ExcludeFiles:      excludeFiles,
			LastUpdated:       time.Now(),
			TotalIndexedFiles: processedFiles,
			TotalFailedFiles:  failedFiles,
		}
		err = ca.saveProjectConfig(ca.projectConfig)
		if err != nil {
			fmt.Printf("Error saving project config: %v\n", err)
		}
	}

	// Cool down configuration
	const (
		maxFileProcessTime  = 30 * time.Second // N seconds per file threshold
		maxTotalProcessTime = 5 * time.Minute  // Y minutes total threshold
		coolDownPeriod      = 60 * time.Second // X seconds to wait
	)

	var (
		startTotalTime = time.Now()
		lastCoolDown   time.Time
	)

	// Create a ticker for periodic saves
	saveTicker := time.NewTicker(30 * time.Second)
	defer saveTicker.Stop()

	go func() {
		for range saveTicker.C {
			ca.projectConfig.LastUpdated = time.Now()
			ca.projectConfig.TotalIndexedFiles = processedFiles
			ca.projectConfig.TotalFailedFiles = failedFiles

			if err := ca.saveProjectConfig(ca.projectConfig); err != nil {
				fmt.Printf("Auto-save error: %v\n", err)
			}
		}
	}()
	isLocalHost := strings.Contains(ca.config.OllamaHost, "localhost") //true
	// Initialize with safe defaults (85°C critical, 65°C safe)
	tempMonitor := NewTemperatureMonitor(80, 65, !isLocalHost)

	bar := progressbar.Default(int64(len(files)))
	for _, file := range files {

		fileStartTime := time.Now()
		// Check if we need cooldown
		if time.Since(lastCoolDown) < coolDownPeriod {
			if tempMonitor.useFallback {
				remaining := coolDownPeriod - time.Since(lastCoolDown)
				fmt.Printf("\nCooling down for %.0f more seconds...\n", remaining.Seconds())
				time.Sleep(remaining)
			} else {
				temp, source, _ := tempMonitor.getTemperature()
				if temp >= tempMonitor.criticalTemp {
					color.Yellow("\n🚨 %s temperature critical (%.1f°C)",
						strings.ToUpper(source), temp)
					if err := tempMonitor.CoolDown(); err != nil {
						color.Red("❌ Cooling failed: %v", err)
					}
				}
			}

		}

		relPath, err := filepath.Rel(path, file)
		if err != nil {
			fmt.Printf("Error getting relative path for %s: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue
		}
		docPath := filepath.Join(projectDocsDir, relPath+".txt")

		// Calculate the MD5 hash of the file
		currentHash, err := calculateMD5Hash(file)
		if err != nil {
			fmt.Printf("Error calculating hash for %s: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue // Skip this file and continue with the next
		}

		// Check if the file has changed
		oldHash, found, err := ca.getFileHash(file)
		if err != nil {
			fmt.Printf("Error getting hash for %s from DB: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue // Skip this file
		}

		if found && oldHash == currentHash {
			bar.Add(1)
			continue // Skip unchanged files
		}
		updatedFiles++ // Increment the number of files to reindex
		fmt.Printf("Processing %s\n", file)
		if err := os.MkdirAll(filepath.Dir(docPath), os.ModePerm); err != nil {
			fmt.Printf("Error creating directory for %s: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue
		}

		code, err := ioutil.ReadFile(file)
		if err != nil {
			fmt.Printf("Error reading file %s: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue
		}

		comments, err := ca.generateComments(string(code))
		if err != nil {
			fmt.Printf("Error generating comments for %s: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue
		}

		if err := ioutil.WriteFile(docPath, []byte(fmt.Sprintf("File: %s\n%s", relPath, comments)), 0644); err != nil {
			fmt.Printf("Error writing doc file for %s: %v\n", file, err)
			failedFiles++
			bar.Add(1)
			continue
		}

		processedFiles++
		bar.Add(1)
		if processedFiles%10 == 0 {
			fmt.Println("Cooling down for 5 seconds")
			time.Sleep(10 * time.Second)
		}

		// Update the file hash in the database
		err = ca.setFileHash(file, currentHash)
		if err != nil {
			fmt.Printf("Error setting hash for %s in DB: %v\n", file, err)
			failedFiles++
		}

		// Check for cooldown triggers after processing each file
		fileProcessTime := time.Since(fileStartTime)
		totalProcessTime := time.Since(startTotalTime)

		if tempMonitor.useFallback && fileProcessTime > maxFileProcessTime || totalProcessTime > maxTotalProcessTime {
			fmt.Printf("\nTriggering cooldown period (file: %.1fs, total: %.1fm)...\n",
				fileProcessTime.Seconds(), totalProcessTime.Minutes())

			lastCoolDown = time.Now()
			time.Sleep(coolDownPeriod)

			// Reset timers after cooldown
			startTotalTime = time.Now()
		} else {
			temp, source, _ := tempMonitor.getTemperature()
			if temp >= tempMonitor.criticalTemp {
				color.Yellow("\n🚨 %s temperature critical (%.1f°C)",
					strings.ToUpper(source), temp)
				if err := tempMonitor.CoolDown(); err != nil {
					color.Red("❌ Cooling failed: %v", err)
				}
			}
		}

	}
	// Save the project config
	ca.projectConfig = ProjectConfig{
		ProjectName:       projectName,
		ProjectPath:       path,
		ExcludeFolders:    exclude,
		ExcludeFiles:      excludeFiles,
		LastUpdated:       time.Now(),
		TotalIndexedFiles: processedFiles,
		TotalFailedFiles:  failedFiles,
	}

	err = ca.saveProjectConfig(ca.projectConfig)
	if err != nil {
		fmt.Printf("Error saving project config: %v\n", err)
	}

	// Save the updated file hashes to the file
	//if err := ca.saveFileHashes(); err != nil {
	//	fmt.Printf("Error saving file hashes: %v\n", err)
	//}
	fmt.Printf("Processed %d new files\n", processedFiles)
	fmt.Printf("%d files failed to process.\n", failedFiles)

	if updatedFiles > 0 {
		fmt.Printf("%d files were updated and need reindexing.\n", updatedFiles)
		//remove the whole vector DB and add code
		os.RemoveAll(ca.config.HashDBPath)
		db, err := chromem.NewPersistentDB(ca.config.HashDBPath, false)

		if err != nil {
			return fmt.Errorf("failed to add document to vector DB: %v", err)
		}
		ca.vectorDB = db

		return ca.createVectorStore(projectName, path)

	}
	return nil
}

func (ca *CodeAssistant) createVectorStore(projectName, codebasePath string) error {
	projectDocsDir := filepath.Join(ca.config.DocsDir, projectName)

	var documents []chromem.Document
	err := filepath.Walk(projectDocsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".txt") {
			content, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}

			relPath := strings.TrimSuffix(strings.TrimPrefix(path, projectDocsDir+string(filepath.Separator)), ".txt")
			originalPath := filepath.Join(codebasePath, relPath)

			documents = append(documents, chromem.Document{
				ID:      relPath,
				Content: string(content),
				Metadata: map[string]string{
					"file_path": originalPath,
				},
			})
		}
		return nil
	})
	if err != nil {
		return err
	}

	bar := progressbar.Default(3)
	bar.Describe("Splitting documents")
	// Simple text splitter (placeholder)
	// splitter := func(text string) []string {
	// 	return strings.Split(text, "\n\n") // Split by double newlines
	// }
	collec, err := ca.vectorDB.CreateCollection(projectName, nil, chromem.NewEmbeddingFuncOllama(ca.config.EmbeddingModel, ""))
	if err != nil {
		return fmt.Errorf("failed to add document to vector DB: %v", err)
	}
	for _, doc := range documents {
		if err := collec.AddDocument(context.Background(), doc); err != nil {
			//{ID:doc.ID, Content:doc, Metadata: doc.Metadata}
			return fmt.Errorf("failed to add document to vector DB: %v", err)
		}
		// chunks := splitter(doc.Content)
		// for _, chunk := range chunks {
		// 	// Add each chunk to the vector DB

		// }
	}
	bar.Add(1)

	bar.Describe("Saving vector store")
	fmt.Printf("Index updated for %s with %d files\n", projectName, len(documents))
	return nil
}

func (ca *CodeAssistant) reindexCodebase() error {
	projects, err := ca.listProjects(ca.config.DocsDir)
	if err != nil {
		return err
	}

	if len(projects) == 0 {
		fmt.Println("No projects available for reindexing")
		return nil
	}

	fmt.Println("Available projects:")
	for i, p := range projects {
		fmt.Printf("%d. %s\n", i+1, p)
	}

	fmt.Print("Select project to reindex: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	choice := scanner.Text()
	selectedIndex := 0
	fmt.Sscanf(choice, "%d", &selectedIndex)
	selectedProject := projects[selectedIndex-1]

	fmt.Printf("Delete ALL data for %s and reindex? (y/n): ", selectedProject)
	scanner.Scan()
	confirm := strings.ToLower(scanner.Text())
	if confirm == "y" {
		docsPath := filepath.Join(ca.config.DocsDir, selectedProject)

		if err := os.RemoveAll(docsPath); err != nil {
			return err
		}
		// Delete file hash entries from the SQLite database for the selected project
		_, err = ca.db.Exec("DELETE FROM file_hashes WHERE file_path LIKE ?", filepath.Join(ca.projectConfig.ProjectPath, "%"))
		if err != nil {
			return fmt.Errorf("failed to delete file hash entries from DB: %v", err)
		}
	}

	fmt.Printf("Reindexing %s...\n", selectedProject)
	return ca.indexCodebase(selectedProject)
}

func (ca *CodeAssistant) searchCodebaseCli() error {
	projects, err := ca.listProjects(ca.config.DocsDir)
	if err != nil {
		return err
	}

	if len(projects) == 0 {
		fmt.Println("No indexed projects found")
		return nil
	}

	fmt.Println("Available projects:")
	for i, p := range projects {
		fmt.Printf("%d. %s\n", i+1, p)
	}

	fmt.Print("Select project: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	choice := scanner.Text()
	selectedIndex := 0
	fmt.Sscanf(choice, "%d", &selectedIndex)
	selectedProject := projects[selectedIndex-1]

	fmt.Printf("Loaded %s. Enter queries (type 'exit' to quit):\n", selectedProject)

	for {
		fmt.Print("\nQuery: ")
		scanner.Scan()
		query := strings.TrimSpace(scanner.Text())
		if strings.ToLower(query) == "exit" {
			break
		}

		fmt.Println("project name is ", selectedProject)
		fmt.Println("query is", query)

		res, err := ca.searchCodebase(selectedProject, query)
		if err != nil {
			fmt.Errorf("error occured: %v", err)
			return err
		}
		fmt.Print(res)
	}

	return nil
}

func (ca *CodeAssistant) searchCodebase(projectName string, query string) (string, error) {
	// Initialize the Ollama client
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return "", fmt.Errorf("failed to create Ollama client: %v", err)
	}
	// Retrieve relevant documents from the vector DB
	collec := ca.vectorDB.GetCollection(projectName, chromem.NewEmbeddingFuncOllama(ca.config.EmbeddingModel, ""))
	results, err := collec.Query(context.Background(), query, 1, nil, nil) // Search for top 5 results
	if err != nil {
		return "", fmt.Errorf("failed to search vector DB: %v", err)
	}
	// Display results
	code := ""
	fmt.Println("\nThinking...")
	for _, result := range results {
		// fmt.Printf("- %s: %s\n", result.ID, result.Content)
		code = code + result.Content
	}
	// Prepare the prompt

	prompt := fmt.Sprintf(`
		Context: %s
		Question: %s
		Answer query clearly and concisely, include relevant file paths when applicable. Your answer should be related to this codebase only`, code, query)

	// fmt.Sprintf("%b", api)
	// fmt.Println("prompt is ", prompt)

	// Create the chat request
	req := &api.ChatRequest{
		Model: ca.config.CodeChatModel,
		Messages: []api.Message{
			{
				Role:    "user",
				Content: prompt,
			},
		},
	}

	var responseContent strings.Builder
	respFunc := func(resp api.ChatResponse) error {
		responseContent.WriteString(resp.Message.Content)
		return nil
	}
	// Send the request to Ollama
	err = client.Chat(context.Background(), req, respFunc)
	if err != nil {
		return "", fmt.Errorf("failed to generate comments: %v", err)
	}

	// Return the generated comments
	return responseContent.String(), nil
}

func (ca *CodeAssistant) listProjects(dir string) ([]string, error) {
	var projects []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			projects = append(projects, entry.Name())
		}
	}
	return projects, nil
}

func (ca *CodeAssistant) runCLI() {
	for {
		fmt.Println("\nCode Assistant Console")
		fmt.Println("1. Index Codebase")
		fmt.Println("2. Search Codebase")
		fmt.Println("3. Reindex Codebase")
		fmt.Println("4. Review Commit")
		fmt.Println("5. Exit")
		fmt.Print("Select option: ")

		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		choice := scanner.Text()

		switch choice {
		case "1":
			if err := ca.indexCodebase(""); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		case "2":
			if err := ca.searchCodebaseCli(); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		case "3":
			if err := ca.reindexCodebase(); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		case "4":
			// fmt.Print("Enter repository path: ")
			// scanner.Scan()
			// repoPath := scanner.Text()
			projects, err := ca.listProjects(ca.config.DocsDir)
			if err != nil {
				fmt.Printf("Error: %v\n", err)
			}

			if len(projects) == 0 {
				fmt.Println("No indexed projects found")
				fmt.Printf("Error: %v\n", err)
			}

			fmt.Println("Available projects:")
			for i, p := range projects {
				fmt.Printf("%d. %s\n", i+1, p)
			}

			fmt.Print("Select project: ")
			scanner := bufio.NewScanner(os.Stdin)
			scanner.Scan()
			choice := scanner.Text()
			selectedIndex := 0
			fmt.Sscanf(choice, "%d", &selectedIndex)
			selectedProject := projects[selectedIndex-1]
			ca.projectConfig, err = ca.loadProjectConfig(selectedProject)

			if err != nil {
				fmt.Printf("Error: %v\n", err)
			}
			repoPath := ca.projectConfig.ProjectPath
			if err := ca.reviewCommit(repoPath); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		case "5":
			fmt.Println("Exiting...")
			return
		default:
			fmt.Println("Invalid choice")
		}
	}
}

// Web UI Handlers
func (ca *CodeAssistant) homeHandler(w http.ResponseWriter, r *http.Request) {
	projects, err := ca.listProjects(ca.config.DocsDir)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error listing projects: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse the template
	tmpl, err := template.ParseFiles("templates/index.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing template: %v", err), http.StatusInternalServerError)
		return
	}

	data := map[string][]string{
		"Projects": projects,
	}

	// Execute the template
	err = tmpl.Execute(w, data)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error executing template: %v", err), http.StatusInternalServerError)
		return
	}
}

func (ca *CodeAssistant) projectHandler(w http.ResponseWriter, r *http.Request) {
	projectName := r.URL.Path[len("/project/"):] // Extract project name from URL
	projectConfig, err := ca.loadProjectConfig(projectName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error loading project config: %v", err), http.StatusInternalServerError)
		return
	}
	// Parse the template
	tmpl, err := template.ParseFiles("templates/project.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing template: %v", err), http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, projectConfig)

	if err != nil {
		http.Error(w, fmt.Sprintf("Error executing template: %v", err), http.StatusInternalServerError)
		return
	}
}

func (ca *CodeAssistant) indexHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("calling index code base")
	err := ca.indexCodebase("") // force to ask code details
	if err != nil {
		http.Error(w, fmt.Sprintf("Error Indexing: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (ca *CodeAssistant) chatHandler(w http.ResponseWriter, r *http.Request) {
	projectName := r.URL.Path[len("/chat/"):]
	// Parse the template
	tmpl, err := template.ParseFiles("templates/chat.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing template: %v", err), http.StatusInternalServerError)
		return
	}
	data := map[string]string{
		"ProjectName": projectName,
	}
	err = tmpl.Execute(w, data)

	if err != nil {
		http.Error(w, fmt.Sprintf("Error executing template: %v", err), http.StatusInternalServerError)
		return
	}
}

func (ca *CodeAssistant) queryHandler(w http.ResponseWriter, r *http.Request) {
	projectName := r.FormValue("project_name")
	query := r.FormValue("query")

	if projectName == "" || query == "" {
		http.Error(w, "Project name and query are required", http.StatusBadRequest)
		return
	}

	response, err := ca.searchCodebase(projectName, query)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error searching codebase: %v", err), http.StatusInternalServerError)
		return
	}

	// Escape the response for HTML to prevent XSS
	escapedResponse := template.HTMLEscapeString(response)

	// Create the HTML response
	htmlResponse := fmt.Sprintf("<p><strong>Query:</strong> %s</p><p><strong>Response:</strong> %s</p>", template.HTMLEscapeString(query), escapedResponse)

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(htmlResponse))
}

func (ca *CodeAssistant) reindexHandler(w http.ResponseWriter, r *http.Request) {
	err := ca.reindexCodebase() // force to ask code details
	if err != nil {
		http.Error(w, fmt.Sprintf("Error Reindexing: %v", err), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// StartWebServer starts the web server
func (ca *CodeAssistant) StartWebServer() {
	http.HandleFunc("/", ca.homeHandler)
	http.HandleFunc("/project/", ca.projectHandler)
	http.HandleFunc("/index", ca.indexHandler)
	http.HandleFunc("/chat/", ca.chatHandler)
	http.HandleFunc("/query", ca.queryHandler)
	http.HandleFunc("/reindex", ca.reindexHandler)

	// Serve static files (CSS, JS, etc.)
	fs := http.FileServer(http.Dir("static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	fmt.Printf("Starting web server on :%s\n", ca.config.WebPort)
	log.Fatal(http.ListenAndServe("0.0.0.0:"+ca.config.WebPort, nil))
}

func (ca *CodeAssistant) loadProjects() error {
	projects, err := ca.listProjects(ca.config.DocsDir)
	if err != nil {
		return err
	}
	ca.projects = projects
	return nil
}

func (ca *CodeAssistant) run() {
	// Load projects at startup
	if err := ca.loadProjects(); err != nil {
		fmt.Printf("Error loading projects: %v\n", err)
	}
	fmt.Println(`
	┌────────────────────────────────────────────┐
	│   CodeSage - The Sage Knows Your Code      │
	│   AI-Powered Code Documentation & Review   │
	└────────────────────────────────────────────┘
	`)
	go ca.StartWebServer()
	ca.runCLI()
}

func main() {
	// Load global configuration
	config, err := LoadConfig("config.json")
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		return
	}

	// Set OLLAMA_HOST environment variable
	os.Setenv("OLLAMA_HOST", config.OllamaHost)

	if err := MakeModelsAvailable(config); err != nil {
		log.Fatalf("Error getting models: %v", err)
	}

	// Initialize and run the code assistant
	assistant := NewCodeAssistant(config)
	if assistant != nil {
		assistant.run()
	}
}
