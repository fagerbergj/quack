import { render, screen, within } from '@testing-library/react';
import { expect, test } from 'vitest';
// The app loads shapes through the extension index; importing it first here would enter the cycle from the wrong end.
import '../index.ts';
import { SHAPES } from './shapes.tsx';
import digest7 from './fixtures/7-digest.json';
import retro7 from './fixtures/7-retro.json';
import history8 from './fixtures/8-history.json';
import trends8 from './fixtures/8-trends.json';
import waivers8 from './fixtures/8-waivers.json';

type Card = Record<string, any>;
const card = (f: { content: string }, edit: (c: Card) => void = () => {}) => {
  const c = JSON.parse(f.content) as Card;
  edit(c);
  return c;
};

test('a retro whose matchup had no second roster states no opponent score and no result', () => {
  render(<SHAPES.retro c={card(retro7, (c) => ((c.opp = null), (c.won = false)))} />);
  expect(screen.queryByText(/to 0/)).toBeNull();
  expect(screen.queryByText(/WON|LOST/)).toBeNull();
  expect(screen.getByText(/^vs I Think/)).toBeInTheDocument();
});

test('a bye-week retro is its summary alone', () => {
  render(<SHAPES.retro c={card(retro7, (c) => ((c.bye = true), (c.summary = 'Bye week, nothing started.')))} />);
  expect(screen.getByText('Bye week, nothing started.')).toBeInTheDocument();
  expect(screen.queryByRole('img')).toBeNull();
});

const waiverItems = (c: Card) => c.items.filter((i: Card) => i.kind === 'waiver');

test('waiver adds rank by count, largest bar first', () => {
  // Reversed, so the feed order is ascending and only a sort puts the largest first.
  render(<SHAPES.trends c={card(trends8, (c) => void c.items.reverse())} />);
  const counts = within(screen.getByText(/· adds$/).parentElement!).getAllByRole('listitem').map((li) => li.lastElementChild!.textContent!);
  const value = (s: string) => Number.parseFloat(s) * (s.includes('M') ? 1e6 : s.includes('k') ? 1e3 : 1);
  expect(counts.length).toBeGreaterThan(1);
  expect(counts.map(value)).toEqual(counts.map(value).toSorted((a, b) => b - a));
});

test('one waiver item without a count turns the group to chips, never a bar with a guessed number', () => {
  render(<SHAPES.trends c={card(trends8, (c) => void (waiverItems(c)[0].text = 'Rising fast in the league.'))} />);
  expect(screen.getByText('Rising on waivers')).toBeInTheDocument();
  expect(screen.queryByText(/· adds$/)).toBeNull();
});

test('a trend kind the labels do not know reads as a sentence, and a player with no position gets no empty tag', () => {
  render(<SHAPES.trends c={card(trends8, (c) => ((c.items[0].kind = 'news'), (c.items[0].player.pos = null)))} />);
  expect(screen.getByText('News')).toBeInTheDocument();
  const chip = screen.getByText(c0Name()).closest('li')!;
  expect(chip.querySelector('.font-mono')).toBeNull();
});
const c0Name = () => (JSON.parse(trends8.content) as Card).items[0].player.name as string;

test('the drop line names the role as the opened card does', () => {
  render(<SHAPES.waivers c={card(waivers8, (c) => void (c.candidates[0].drop.role = 'bench'))} />);
  expect(screen.getByText(/· Bench$/)).toBeInTheDocument();
});

test('a digest with no game of yours lists every game and no scoreline', () => {
  render(<SHAPES.digest c={card(digest7, (c) => c.games.forEach((g: Card) => (g.mine = false)))} />);
  expect(screen.queryByText('vs')).toBeNull();
  expect(within(screen.getByText('Other games').parentElement!).getAllByRole('listitem')).toHaveLength(5);
});

test('standings with no team inside the playoff line draw no line', () => {
  render(<SHAPES.digest c={card(digest7, (c) => c.playoff_line.standings.forEach((s: Card) => (s.in_playoff_line = false)))} />);
  expect(screen.queryByRole('separator', { name: 'Playoff line' })).toBeNull();
});

test('a week with no result is marked in the season strip label, so the count matches the cells', () => {
  render(<SHAPES.history c={card(history8, (c) => void (c.seasons[0].weeks[1].result = null))} />);
  expect(screen.getByRole('img', { name: /week by week: L – L W/ })).toBeInTheDocument();
});
