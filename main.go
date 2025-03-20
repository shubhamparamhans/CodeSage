package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/ollama/ollama/api"
	"github.com/schollz/progressbar/v3"
	"github.com/philippgille/chromem-go"// Chromem in-memory vector DB
)

// Config holds the configuration values
type Config struct {
	DocsDir            string `json:"docs_dir"`
	EmbeddingModel     string `json:"embedding_model"`
	CodeChatModel      string `json:"code_chat_model"`
	DocumentationModel string `json:"documentation_model"`
	OllamaHost         string `json:"ollama_host"`
}

// DefaultConfig returns the default configuration
func DefaultConfig() Config {
	return Config{
		DocsDir:            "./docs",
		EmbeddingModel:     "nomic-embed-text",
		CodeChatModel:      "qwen2.5-coder:1.5b",
		DocumentationModel: "llama3.2:1b",
		OllamaHost:         "http://localhost:11434",
	}
}

// LoadConfig loads the configuration from a file or creates it with default values
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

		fmt.Printf("Config file created: %s\nPlease update the variables and rerun the program.\n", filename)
		os.Exit(0)
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
type CodeAssistant struct {
	vectorDB *chromem.DB // Chromem in-memory vector DB
	config   Config      // Configuration values
}

func NewCodeAssistant(config Config) *CodeAssistant {
	// Initialize Chromem in-memory vector DB
	db, err := chromem.NewPersistentDB("./db", false)
	if err != nil {
		fmt.Printf("failed to create Ollama client: %v\n", err)
		return nil
	}

	return &CodeAssistant{
		vectorDB: db,
		config:   config,
	}
}


func (ca *CodeAssistant) parseDirectory(directoryPath string, excludeDirs []string) ([]string, error) {
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
			ext := filepath.Ext(path)
			if ext == ".py" || ext == ".js" || ext == ".ts" || ext == ".java" || ext == ".cpp" || ext == ".c" || ext == ".go" || ext == ".vue"{
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

func (ca *CodeAssistant) indexCodebase(reindexProject string) error {
	var projectName string
	if reindexProject != "" {
		projectName = reindexProject
	} else {
		fmt.Print("Enter project name: ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		projectName = scanner.Text()
	}

	fmt.Print("Enter codebase path: ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	path := scanner.Text()

	fmt.Print("Folders to exclude (comma separated): ")
	scanner.Scan()
	exclude := []string{}
	temp := scanner.Text()
	if len(temp) > 0  {
		exclude = strings.Split(temp, ",")
	}
	exclude = append(exclude, "/node_modules","/venv","/build","/dist","/.venv","/log","/node_modules/","/venv/","/build/","/dist/","/.venv/","/log/","/.vite/","/.git/")
	for i := range exclude {
		exclude[i] = strings.TrimSpace(exclude[i])
	}

	projectDocsDir := filepath.Join(ca.config.DocsDir, projectName)
	files, err := ca.parseDirectory(path, exclude)
	if err != nil {
		return err
	}

	fmt.Printf("Indexing %d files...\n", len(files))
	processedFiles := 0

	bar := progressbar.Default(int64(len(files)))
	for _, file := range files {
		relPath, err := filepath.Rel(path, file)
		if err != nil {
			return err
		}
		docPath := filepath.Join(projectDocsDir, relPath+".txt")

		if _, err := os.Stat(docPath); err == nil {
			bar.Add(1)
			continue // Skip already processed files
		}

		fmt.Printf("Processing %s\n", file)
		if err := os.MkdirAll(filepath.Dir(docPath), os.ModePerm); err != nil {
			return err
		}

		code, err := ioutil.ReadFile(file)
		if err != nil {
			return err
		}

		comments, err := ca.generateComments(string(code))
		if err != nil {
			return err
		}

		if err := ioutil.WriteFile(docPath, []byte(fmt.Sprintf("File: %s\n%s", relPath, comments)), 0644); err != nil {
			return err
		}

		processedFiles++
		bar.Add(1)
	}

	fmt.Printf("Processed %d new files\n", processedFiles)
	return ca.createVectorStore(projectName, path)
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
	collec,err := ca.vectorDB.CreateCollection("codeio",nil,chromem.NewEmbeddingFuncOllama(ca.config.EmbeddingModel, ""))
	if err!= nil{
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
	if confirm != "y" {
		return nil
	}

	docsPath := filepath.Join(ca.config.DocsDir, selectedProject)
	if err := os.RemoveAll(docsPath); err != nil {
		return err
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
		collec := ca.vectorDB.GetCollection("codeio",chromem.NewEmbeddingFuncOllama(ca.config.EmbeddingModel, ""))
		results, err := collec.Query(context.Background(), query, 1,nil,nil) // Search for top 5 results
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
		Answer query clearly and concisely, include relevant file paths when applicable. Your answer should be related to this codebase only`, code,query)

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
	// Load configuration
	config, err := LoadConfig("config.json")
	if err != nil {
		fmt.Printf("Failed to load config: %v\n", err)
		return
	}

	// Set OLLAMA_HOST environment variable
	os.Setenv("OLLAMA_HOST", config.OllamaHost)

	// Initialize and run the code assistant
	assistant := NewCodeAssistant(config)
	if assistant != nil {
		assistant.run()
	}
}
