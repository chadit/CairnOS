interface SpinnerProps {
  label?: string;
}

/** Spinner is the shared loading indicator with an accessible status label. */
export function Spinner({ label = 'Loading' }: SpinnerProps) {
  return (
    <div role="status" className="flex items-center gap-2 text-sm text-gray-500">
      <span
        aria-hidden="true"
        className="h-4 w-4 animate-spin rounded-full border-2 border-gray-300 border-t-gray-600"
      />
      <span>{label}</span>
    </div>
  );
}
