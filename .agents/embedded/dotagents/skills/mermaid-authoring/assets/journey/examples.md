# User Journey examples

## Onboarding a new user

What this shows: a single-actor journey across sequential stages, the most common use — spotting the low-satisfaction step (account setup) that needs fixing.

```mermaid
journey
    title New User Onboarding
    section Sign up
      Create account: 3: Me
      Verify email: 2: Me
    section First use
      Explore dashboard: 4: Me
      Complete first task: 5: Me
```

## Support call with multiple actors

What this shows: tasks shared between actors (customer and agent both present on one step), useful when a journey isn't solo.

```mermaid
journey
    title Support Call
    section Reach out
      Call support line: 2: Customer
      Wait on hold: 1: Customer
    section Resolve
      Explain issue: 3: Customer, Agent
      Get fix: 5: Customer, Agent
```
