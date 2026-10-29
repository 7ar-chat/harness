/** The har logo: a red jalapeño. Decorative; the name next to it labels it. */
export function Logo({ className = "size-5" }: { className?: string }) {
  return (
    <svg viewBox="0 0 32 32" className={className} aria-hidden="true" focusable="false">
      <path d="M9.5 10.5c-3 6.5-1 15.5 7.5 18.5 4.5 1.6 8-1.2 5-3.4-5-2.2-6.2-8.2-4-13.6z" fill="#d62828" />
      <path d="M12.5 13c-1.2 4.6.2 10 4.6 12.6" fill="none" stroke="#f77f7f" strokeWidth="1.6" strokeLinecap="round" />
      <path d="M9.5 10.5c.8-3 4-4.3 7.4-3.2l-1.2 3.6z" fill="#2f7d32" />
      <path d="M15.8 7.6c-.2-2 .9-3.6 3-4.4" fill="none" stroke="#2f7d32" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}
