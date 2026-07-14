package workflow

// Validate checks structural invariants the executor relies on never having
// to re-check at run time: exactly one start step, every edge target
// exists, conditional steps have both branches, every step with no
// outgoing edge is an end step (so execution always stops at a deliberate
// terminus, never by silently running off the graph), no cycles, and every
// step is reachable from start (catches dead, unreachable nodes left over
// from editing). Called once, whenever a workflow is created or updated —
// see service.go.
func (d Definition) Validate() error {
	if len(d.Steps) == 0 {
		return ErrEmptyDefinition
	}

	byID := make(map[string]Step, len(d.Steps))
	startCount := 0
	for _, s := range d.Steps {
		if s.ID == "" {
			return ErrInvalidStepConfig
		}
		if _, exists := byID[s.ID]; exists {
			return ErrDuplicateStepID
		}
		byID[s.ID] = s
		if s.Type == StepStart {
			startCount++
		}
	}
	if startCount != 1 {
		return ErrMissingStartStep
	}

	for _, s := range d.Steps {
		edges := outgoingEdges(s)
		for _, next := range edges {
			if _, ok := byID[next]; !ok {
				return ErrDanglingEdge
			}
		}
		if s.Type == StepConditional && (s.NextIfTrue == "" || s.NextIfFalse == "") {
			return ErrConditionalMissingBranch
		}
		if len(edges) == 0 && s.Type != StepEnd {
			return ErrDeadEnd
		}
		if s.Type == StepEnd && len(edges) != 0 {
			return ErrEndHasOutgoing
		}
	}

	start, _ := d.startStep() // startCount==1 already guarantees this exists
	visited := make(map[string]bool, len(d.Steps))
	if err := detectCycle(start.ID, byID, visited, make(map[string]bool)); err != nil {
		return err
	}
	for _, s := range d.Steps {
		if !visited[s.ID] {
			return ErrUnreachableStep
		}
	}
	return nil
}

// outgoingEdges returns a step's active edges: a conditional step branches
// on NextIfTrue/NextIfFalse, every other step type has at most one Next —
// there is no fan-out node type in this simplified engine, so a step never
// has more than two outgoing edges.
func outgoingEdges(s Step) []string {
	if s.Type == StepConditional {
		var edges []string
		if s.NextIfTrue != "" {
			edges = append(edges, s.NextIfTrue)
		}
		if s.NextIfFalse != "" {
			edges = append(edges, s.NextIfFalse)
		}
		return edges
	}
	if s.Next != "" {
		return []string{s.Next}
	}
	return nil
}

// detectCycle is a standard DFS with a recursion-stack set (stack) to catch
// back-edges. visited is shared across the whole traversal (not reset per
// branch) and doubles as the reachability record Validate checks
// afterwards — a cycle-free DFS from start visits exactly the set of
// reachable steps, so one traversal answers both questions.
func detectCycle(id string, byID map[string]Step, visited, stack map[string]bool) error {
	if stack[id] {
		return ErrCycleDetected
	}
	if visited[id] {
		return nil
	}
	visited[id] = true
	stack[id] = true
	for _, next := range outgoingEdges(byID[id]) {
		if err := detectCycle(next, byID, visited, stack); err != nil {
			return err
		}
	}
	stack[id] = false
	return nil
}
