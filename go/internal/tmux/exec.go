package tmux

import (
    "bytes"
    "context"
    "encoding/base64"
    "fmt"
    "os/exec"
    "strings"
)

// BuildPath merges PATH additions without duplicates.
func BuildPath(current string, additions []string) string {
    parts := []string{}
    if current != "" {
        parts = append(parts, strings.Split(current, ":")...)
    }
    for _, a := range additions {
        if a == "" {
            continue
        }
        found := false
        for _, p := range parts {
            if p == a {
                found = true
                break
            }
        }
        if !found {
            parts = append(parts, a)
        }
    }
    return strings.Join(parts, ":")
}

// Run executes tmux locally or via ssh with a base64-wrapped command to protect format strings.
func Run(ctx context.Context, host string, tmuxBin string, pathAdd []string, args []string) (string, error) {
    basePath := BuildPath("", pathAdd)
    quotedArgs := make([]string, 0, len(args)+2)
    quotedArgs = append(quotedArgs, tmuxBin)
    for _, a := range args {
        quotedArgs = append(quotedArgs, shQuote(a))
    }
    commandStr := fmt.Sprintf("PATH=%s exec %s", basePath, strings.Join(quotedArgs, " "))
    b64 := base64.StdEncoding.EncodeToString([]byte(commandStr))
    remoteCmd := fmt.Sprintf("printf %s %s | base64 -d | sh", "%s", shQuote(b64))

    var cmd *exec.Cmd
    if host != "" {
        cmd = exec.CommandContext(ctx, "ssh", "-T", host, remoteCmd)
    } else {
        // local: no ssh, but keep same execution model
        cmd = exec.CommandContext(ctx, "sh", "-c", commandStr)
    }
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    if err := cmd.Run(); err != nil {
        return stdout.String(), fmt.Errorf("tmux run failed: %w: %s", err, stderr.String())
    }
    return strings.TrimRight(stdout.String(), "\n"), nil
}

// shQuote returns a single-quoted shell literal.
func shQuote(s string) string {
    if s == "" {
        return "''"
    }
    return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
