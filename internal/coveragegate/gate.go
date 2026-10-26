// Package coveragegate checks exact integer coverage, without rounding or exclusions.
package coveragegate

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Check merges repeated instrumentation blocks across test packages exactly as
// go tool cover does for -coverpkg=./.... Every repository package remains included.
func Check(input io.Reader) error {
	scanner := bufio.NewScanner(input)
	if !scanner.Scan() {
		return errors.New("missing coverage mode")
	}
	switch scanner.Text() {
	case "mode: set", "mode: count", "mode: atomic":
	default:
		return errors.New("invalid coverage mode")
	}
	type block struct {
		statements uint64
		hit        bool
	}
	blocks := make(map[string]block)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 {
			return errors.New("invalid coverage block")
		}
		statements, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return errors.New("invalid statement count")
		}
		count, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return errors.New("invalid execution count")
		}
		previous, exists := blocks[fields[0]]
		if exists && previous.statements != statements {
			return errors.New("inconsistent instrumentation")
		}
		blocks[fields[0]] = block{statements, previous.hit || count > 0}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(blocks) == 0 {
		return errors.New("empty coverage profile")
	}
	for name, block := range blocks {
		if !block.hit {
			return fmt.Errorf("uncovered block: %s", name)
		}
	}
	return nil
}
