import type { ReactNode, SVGProps } from 'react';

/** The quack duck, facing left; `failed` crosses out its eye. Decorative unless given an aria-label. */
export function Duck({ failed, ...svg }: SVGProps<SVGSVGElement> & { failed?: boolean }) {
  return (
    <svg viewBox="0 0 64 48" aria-hidden={svg['aria-label'] ? undefined : true} {...svg}>
      <ellipse cx="34" cy="32" rx="22" ry="11" fill="#fdfcf7" stroke="#2f3a36" strokeOpacity=".25" />
      <circle cx="20" cy="18" r="10" fill="#1f7a63" />
      <rect x="11" y="25" width="18" height="3" rx="1.5" fill="#fff" />
      <path d="M12 15 L1 19 L12 22 Z" fill="#e3a92b" />
      {failed
        ? <path d="M14 13 l6 4 M20 13 l-6 4" stroke="#0b0f0e" strokeWidth="1.8" strokeLinecap="round" />
        : <><circle cx="17" cy="15" r="1.9" fill="#0f1513" /><circle cx="16.4" cy="14.4" r="0.6" fill="#fff" /></>}
      <path d="M50 28 Q60 24 58 34" fill="#fdfcf7" />
    </svg>
  );
}

const reed = 'M8 58 Q6 30 9 6';
const duckling = (
  <svg width="20" height="16" viewBox="0 0 18 14">
    <ellipse cx="10" cy="9" rx="7" ry="4.5" fill="#e9c46a" />
    <circle cx="5.5" cy="5" r="3.5" fill="#e9c46a" />
    <path d="M2.4 4.3 L0 5.2 L2.4 6.2 Z" fill="#d08a1d" />
    <circle cx="4.4" cy="4.2" r="0.8" fill="#0f1513" />
  </svg>
);

// The swimmer's head and bill point left in the source art; .swimmer flips it to face its rightward travel.
export function Pond({ children, still }: { children: ReactNode; still?: boolean }) {
  return (
    <div className={`pond ${still ? 'still' : ''} relative overflow-hidden rounded-[28px] border border-line px-6 pt-6 pb-20`}>
      <div className="relative z-10">{children}</div>
      <div aria-hidden="true" className="pointer-events-none absolute inset-x-0 bottom-0 h-[70px]">
        <svg className="reed absolute bottom-3 left-4" width="30" height="58" viewBox="0 0 30 58">
          <path d={reed} stroke="#3f7d4f" strokeWidth="2.5" fill="none" />
          <rect x="6.5" y="2" width="5" height="16" rx="2.5" fill="#7a4a32" />
          <path d="M18 58 Q20 34 17 16" stroke="#4f9461" strokeWidth="2" fill="none" />
        </svg>
        <svg className="reed absolute right-6 bottom-3" style={{ animationDelay: '-1.5s' }} width="26" height="50" viewBox="0 0 30 58">
          <path d={reed} stroke="#3f7d4f" strokeWidth="2.5" fill="none" />
          <rect x="6.5" y="2" width="5" height="16" rx="2.5" fill="#7a4a32" />
        </svg>
        <div className="swimmer flex items-end gap-1">
          <Duck width="46" height="34" />
          <span className="mb-1 flex gap-1">{duckling}{duckling}{duckling}</span>
        </div>
        <div className="waves back absolute inset-x-0 bottom-0" />
        {still && <span className="absolute bottom-[52px] left-[calc(34%+34px)] grid h-[22px] w-[22px] place-items-center rounded-full bg-warning text-[14px] leading-none font-bold text-canvas">!</span>}
        <div className="waves absolute inset-x-0 bottom-0" />
      </div>
    </div>
  );
}
