package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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

	bar := progressbar.Default(int64(len(files)))
	for _, file := range files {
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

		// Update the file hash in the database
		err = ca.setFileHash(file, currentHash)
		if err != nil {
			fmt.Printf("Error setting hash for %s in DB: %v\n", file, err)
			failedFiles++
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
	collec, err := ca.vectorDB.CreateCollection("codeio", nil, chromem.NewEmbeddingFuncOllama(ca.config.EmbeddingModel, ""))
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
	}

	fmt.Printf("Reindexing %s...\n", selectedProject)
	return ca.indexCodebase(selectedProject)
}

func (ca *CodeAssistant) searchCodebase() error {
	// Initialize the Ollama client
	client, err := api.ClientFromEnvironment()
	if err != nil {
		return fmt.Errorf("failed to create Ollama client: %v", err)
	}

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

		// Retrieve relevant documents from the vector DB
		collec := ca.vectorDB.GetCollection("codeio", chromem.NewEmbeddingFuncOllama(ca.config.EmbeddingModel, ""))
		results, err := collec.Query(context.Background(), query, 1, nil, nil) // Search for top 5 results
		if err != nil {
			return fmt.Errorf("failed to search vector DB: %v", err)
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
			return fmt.Errorf("failed to generate comments: %v", err)
		}

		// Return the generated comments
		fmt.Printf(responseContent.String())
		// print(responseContent.String())
		// comments, err := ca.generateComments(string(code))
		// if err != nil {
		// 	return err
		// }
	}
	return nil
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

func (ca *CodeAssistant) run() {
	for {
		fmt.Println("\nCode Assistant Console")
		fmt.Println("1. Index Codebase")
		fmt.Println("2. Search Codebase")
		fmt.Println("3. Reindex Codebase")
		fmt.Println("4. Exit")
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
			if err := ca.searchCodebase(); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		case "3":
			if err := ca.reindexCodebase(); err != nil {
				fmt.Printf("Error: %v\n", err)
			}
		case "4":
			fmt.Println("Exiting...")
			return
		default:
			fmt.Println("Invalid choice")
		}
	}
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
