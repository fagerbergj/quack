# ZenUML examples

## Reply from a nested level with `@return`

What this shows: using `@return` to short-circuit a reply back up to the top-level caller from inside a nested conditional — the case the plain `return`/assignment forms can't express, since those only return to the immediate caller.

```mermaid
zenuml
    title Reply message
    Client->A.method() {
      B.method() {
        if(condition) {
          return x1
          // return early
          @return
          A->Client: x11
        }
      }
      return x2
    }
```

## Try/catch/finally around an external call

What this shows: modeling failure handling in a sequence — a booking flow that rolls back on failure — using ZenUML's code-like exception block instead of mermaid's native `alt`/`opt` fragments.

```mermaid
zenuml
    try {
      Consumer->API: Book something
      API->BookingService: Start booking process
    } catch {
      API->Consumer: show failure
    } finally {
      API->BookingService: rollback status
    }
```

## Parallel actions

What this shows: `par` for actions that happen concurrently rather than sequentially — two notifications fired at once rather than one after another.

```mermaid
zenuml
    par {
        Alice->Bob: Hello guys!
        Alice->John: Hello guys!
    }
```
