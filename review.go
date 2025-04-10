package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
)

func (ca *CodeAssistant) reviewCommit(repoPath string) error {
	commits, err := getCommitList(repoPath)
	if err != nil {
		return fmt.Errorf("failed to get commit list: %v", err)
	}

	fmt.Println("\nRecent Commits:")
	for i, commit := range commits {
		fmt.Printf("%d. %s\n", i+1, commit[:8])
	}

	fmt.Print("\nSelect commit to review (number): ")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Scan()
	choice, _ := strconv.Atoi(scanner.Text())

	if choice < 1 || choice > len(commits) {
		return fmt.Errorf("invalid commit selection")
	}

	diff, err := getGitDiff(repoPath, commits[choice-1])
	if err != nil {
		return fmt.Errorf("failed to get diff: %v", err)
	}

	review, err := ca.generateCodeReview(diff)
	if err != nil {
		return err
	}

	fmt.Println("\nCode Review:")
	fmt.Println(review)
	return nil
}

func (ca *CodeAssistant) generateCodeReview(diff string) (string, error) {
	prompt := fmt.Sprintf(`Review the following code changes and provide:
1. Potential bugs or issues
2. Code style improvements
3. Security concerns
4. Performance optimizations

Code diff:
%s

Provide concise, actionable feedback:`, diff)

	// Use existing generateComments infrastructure
	return ca.generateComments(prompt)
}
