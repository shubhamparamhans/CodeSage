package main

import (
	"bytes"
	"os/exec"
	"strings"
)

func getGitDiff(repoPath string, commitHash string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "diff", commitHash+"^!", "--unified=0")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

func getCommitList(repoPath string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoPath, "log", "--pretty=format:%H", "-n", "20")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}
	return strings.Split(out.String(), "\n"), nil
}
