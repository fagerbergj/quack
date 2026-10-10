package vetting

// probeScope: logger for gate-probe execute_tool ledger events (augmentFromRepo, checks).
const probeScope = "quack.vetting"

// probeRound: fixed ledger round for probes (no runID - re-reads disk per activity() call).
const probeRound = "gate-probe"
