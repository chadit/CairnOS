interface StatusBadgeProps {
  status: string;
}

const COLORS: Record<string, string> = {
  pending: 'bg-gray-100 text-gray-700',
  ready: 'bg-blue-100 text-blue-700',
  running: 'bg-amber-100 text-amber-800',
  succeeded: 'bg-green-100 text-green-700',
  failed: 'bg-red-100 text-red-700',
  skipped: 'bg-gray-100 text-gray-500',
  canceled: 'bg-gray-200 text-gray-700',
};

/** StatusBadge colors a run or action status consistently across views. */
export function StatusBadge({ status }: StatusBadgeProps) {
  const color = COLORS[status] ?? 'bg-gray-100 text-gray-700';
  return (
    <span className={`inline-block rounded px-2 py-0.5 text-xs font-medium ${color}`}>
      {status}
    </span>
  );
}
