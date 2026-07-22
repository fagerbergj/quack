# Recipes: code templates

## Component test (BDD structure)

```tsx
import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@/test-utils';
import userEvent from '@testing-library/user-event';
import { MyComponent } from './MyComponent';

describe('MyComponent', () => {
  it('renders the heading and primary button', () => {
    render(<MyComponent title="Hello" />);
    expect(screen.getByRole('heading', { name: /hello/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /click/i })).toBeInTheDocument();
  });

  it('calls the callback when clicked', async () => {
    const onClick = vi.fn();
    render(<MyComponent title="Hello" onClick={onClick} />);
    await userEvent.click(screen.getByRole('button', { name: /click/i }));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it('shows a success message after the action completes', async () => {
    render(<MyComponent title="Hello" />);
    await userEvent.click(screen.getByRole('button'));
    expect(await screen.findByText(/success/i)).toBeInTheDocument();
  });
});
```

## userEvent vs fireEvent

| Aspect | `fireEvent` | `userEvent` |
|---|---|---|
| Level | Low-level: fires one DOM event | High-level: full browser event sequence |
| Typing | Only the event passed | Per-character keyboard + input events |
| Clicking | Only `click` | pointerdown → mousedown → focus → mouseup → click |
| Async | Sync (void) | Async - `await` each interaction |
| Best for | Custom `CustomEvent`, isolated handler, non-interactive nodes | Anything a real person does |

```tsx
const user = userEvent.setup();
await user.type(screen.getByLabelText(/email/i), 'hello@example.com');
await user.tab();
await user.click(screen.getByRole('button', { name: /submit/i }));
```

## Query families

| Family | 0 matches | 1 | >1 | Async |
|---|---|---|---|---|
| `getBy*`   | throws | element | throws | no |
| `queryBy*` | `null` | element | throws | no |
| `findBy*`  | throws after timeout | element | throws | yes (Promise) |

## Forms

```tsx
it('submits with valid data', async () => {
  const onSubmit = vi.fn();
  render(<LoginForm onSubmit={onSubmit} />);
  await userEvent.type(screen.getByLabelText(/email/i), 'user@example.com');
  await userEvent.type(screen.getByLabelText(/password/i), 'secret123');
  await userEvent.click(screen.getByRole('button', { name: /sign in/i }));
  expect(onSubmit).toHaveBeenCalledWith({ email: 'user@example.com', password: 'secret123' });
});
```

## Custom render with providers (`src/test-utils.tsx`)

```tsx
import { render, RenderOptions } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import React from 'react';

const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const AllTheProviders = ({ children }: { children: React.ReactNode }) => (
  <QueryClientProvider client={client}>{children}</QueryClientProvider>
);
const customRender = (ui: React.ReactElement, options?: Omit<RenderOptions, 'wrapper'>) =>
  render(ui, { wrapper: AllTheProviders, ...options });

export * from '@testing-library/react';
export { customRender as render };
```

## Async patterns

```tsx
// appears after async work
expect(await screen.findByText(/Alice/i)).toBeInTheDocument();

// non-DOM assertion (mock call count)
await waitFor(() => expect(fetchUsers).toHaveBeenCalledTimes(1));

// loading → loaded
await waitForElementToBeRemoved(() => screen.queryByText('Loading...'));
expect(screen.getByText(/Alice/i)).toBeInTheDocument();
```

## Mocking

```tsx
const onToggle = vi.fn();
expect(onToggle).toHaveBeenCalledWith(true);

const spy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);

vi.mock('@/api/client', () => ({
  fetchUser: vi.fn().mockResolvedValue({ id: 1, name: 'Alice' }),
}));
// Jest: identical with jest.fn / jest.spyOn / jest.mock.
```

## Custom hooks + React Query

```tsx
import { renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

const createWrapper = (client: QueryClient) => ({ children }: any) => (
  <QueryClientProvider client={client}>{children}</QueryClientProvider>
);

test('fetches and caches data', async () => {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const { result } = renderHook(() => useFetchData(), { wrapper: createWrapper(client) });
  await waitFor(() => expect(result.current.isSuccess).toBe(true));
  expect(result.current.data).toEqual({ answer: 42 });
});
```

Use MSW for network-level mocking of full data-fetching flows.

## Suspense

```tsx
render(
  <Suspense fallback={<span>Loading…</span>}>
    <UserProfile />
  </Suspense>
);
expect(screen.getByText(/loading/i)).toBeInTheDocument();         // fallback
expect(await screen.findByText(/Alice/i)).toBeInTheDocument();    // after resolve
```
