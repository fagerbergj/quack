export type Line = { price: number; qty: number };

/** The order total in dollars: SAVE10 takes 10% off, the total never goes below zero, and it rounds to cents. */
export function total(lines: Line[], coupon?: string): number {
  let sum = 0;
  for (const l of lines) {
    if (l.qty <= 0) continue;
    sum += l.price * l.qty;
  }
  if (coupon === 'SAVE10') sum = sum * 0.9;
  return Math.max(0, Math.round(sum * 100) / 100);
}
