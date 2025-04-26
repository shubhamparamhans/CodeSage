# CodeSage: AI-Powered Code  Assistant Engineering Manger
**The Sage Knows Your Code**

![Associate AI EM Logo](https://s3.ca-central-1.amazonaws.com/logojoy/logos/215622747/no_padding.png?8606.300000011921) <!-- Add a logo if you have one -->

CodeSage (previously Associate AI EM) is a powerful tool that automates code documentation and indexing. 
It uses AI to generate detailed comments and documentation for your codebase, making it easier to understand and maintain. 
With support for multiple programming languages and an in-memory vector database, Associate AI EM ensures fast and efficient codebase indexing and search functionality.

---

## Features

- **Automated Code Documentation**: Generate detailed comments and documentation for your codebase using AI.
- **Multi-Language Support**: Works with Python, JavaScript, TypeScript, Java, C++, and C.
- **In-Memory Vector Database**: Uses `chromem-go` for fast and efficient codebase indexing and search.
- **Exclusion Filters**: Exclude specific directories and file extensions (e.g., `.log`, `.tmp`) from indexing.
- **Interactive CLI**: Easy-to-use command-line interface for indexing, searching, and reindexing codebases.
- **Persistent Storage**: Save and load indexed data for future use.

---

## Installation

1. Clone the repository:
   ```bash
   git clone https://github.com/shubhamparamhans/CodeSage.git
   cd Associate-AI-EM
---
**Screenshots**
![image](https://github.com/user-attachments/assets/3e3fc85e-4789-4cf3-8262-6ea0e8a98140)

![image](https://github.com/user-attachments/assets/07448c14-181b-46c9-ad91-dc818b8b2a5b)

## How to Use
- Clone repo
- Run code (go run main.go)
- At first it will generate a config file in same folder
  {
  "docs_dir": "./docs",   #your default directory to store data
  "embedding_model": "nomic-embed-text", #text embedding model
  "code_chat_model": "qwen2.5-coder:1.5b", #ollama model to query code
  "documentation_model": "llama3.2:1b", #ollama model to create documentation
  "ollama_host": "http://localhost:11434" #url where your ollama is running
  }

 Ideas/Suggestions for the Codebase**

Here are some smart ideas and suggestions to enhance

1. **Web-Based Interface**:
   - Build a web-based UI using a framework like React or Vue.js for easier interaction with the tool.

2. **Git Integration**:
   - Add support for indexing codebases directly from Git repositories. This could include features like:
     - Indexing only the changes in a specific commit or branch.
     - Automatically reindexing when new commits are pushed.

3. **Custom Documentation Templates**:
   - Allow users to define custom templates for documentation generation. For example:
     - Different styles for function-level comments.
     - Custom headers and footers for generated documentation files.

4. **Distributed Indexing**:
   - Add support for distributed indexing to handle large codebases more efficiently. This could involve:
     - Splitting the codebase into smaller chunks and indexing them in parallel.
     - Using a distributed database for storing the index.

5. **AI Model Fine-Tuning**:
   - Allow users to fine-tune the AI model used for documentation generation. This could involve:
     - Training the model on a specific codebase for better results.
     - Adding support for custom prompts and instructions.

6. **Codebase Health Metrics**:
   - Add functionality to analyze the health of a codebase. For example:
     - Calculate metrics like code complexity, duplication, and test coverage.
     - Generate reports and visualizations for these metrics.

7. **Integration with IDEs**:
   - Build plugins for popular IDEs (e.g., VS Code, IntelliJ) to integrate CodePilot directly into the development workflow.

8. **Community-Driven Extensions**:
   - Create a marketplace for community-driven extensions. For example:
     - Extensions for additional programming languages.
     - Custom documentation templates and styles.

---

### **Next Steps**

**Share with the Community**:
   - Share the repository on social media, forums, and developer communities to gather feedback and contributions.

Let me know if you need further assistance! 🚀
