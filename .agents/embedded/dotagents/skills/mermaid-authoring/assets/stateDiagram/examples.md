# State diagram examples

## Order lifecycle

What this shows: a linear-ish lifecycle with a cancellation path back to a shared end state.

```mermaid
stateDiagram-v2
    [*] --> Pending
    Pending --> Paid : payment received
    Pending --> Cancelled : customer cancels
    Paid --> Shipped : warehouse ships
    Shipped --> Delivered : carrier confirms
    Delivered --> [*]
    Cancelled --> [*]
```

## CI job with a choice branch

What this shows: `<<choice>>` to model a conditional outcome.

```mermaid
stateDiagram-v2
    state test_result <<choice>>
    [*] --> Running
    Running --> test_result
    test_result --> Passed : if exit code == 0
    test_result --> Failed : if exit code != 0
    Passed --> [*]
    Failed --> [*]
```

## Connection retry with a composite state

What this shows: nesting internal retry states inside a parent `Connecting` state.

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Connecting : connect()
    Connecting --> Connected : success
    Connecting --> Idle : give up

    state Connecting {
        [*] --> Attempting
        Attempting --> Backoff : failed
        Backoff --> Attempting : retry
    }
    Connected --> [*]
```

## Feature flag rollout with fork/join

What this shows: `<<fork>>`/`<<join>>` to model parallel rollout stages that must all complete before continuing.

```mermaid
stateDiagram-v2
    state rollout_fork <<fork>>
    [*] --> rollout_fork
    rollout_fork --> CanaryRegion
    rollout_fork --> BetaUsers

    state rollout_join <<join>>
    CanaryRegion --> rollout_join
    BetaUsers --> rollout_join
    rollout_join --> FullRollout
    FullRollout --> [*]
```

## Keyboard lock concurrency

What this shows: `--` to model independent concurrent regions inside one composite state (adapted from the mermaid docs' own keyboard-lock example).

```mermaid
stateDiagram-v2
    [*] --> Active

    state Active {
        [*] --> NumLockOff
        NumLockOff --> NumLockOn : EvNumLockPressed
        NumLockOn --> NumLockOff : EvNumLockPressed
        --
        [*] --> CapsLockOff
        CapsLockOff --> CapsLockOn : EvCapsLockPressed
        CapsLockOn --> CapsLockOff : EvCapsLockPressed
    }
```
