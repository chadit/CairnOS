import { type FormEvent, type KeyboardEvent, useState } from 'react';

interface ComposerProps {
  disabled: boolean;
  onSend: (text: string) => void;
}

/**
 * Composer is the message input. Enter sends; Shift+Enter inserts a newline so
 * a multi-line message is still possible. The textarea has a visible label for
 * assistive tech and the send control is a real submit button.
 */
export function Composer({ disabled, onSend }: ComposerProps) {
  const [text, setText] = useState('');

  function submit() {
    const trimmed = text.trim();
    if (trimmed.length === 0) {
      return;
    }
    onSend(trimmed);
    setText('');
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault();
    submit();
  }

  function handleKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      submit();
    }
  }

  return (
    <form onSubmit={handleSubmit} className="flex items-end gap-2">
      <label className="flex-1 space-y-1 text-sm">
        <span className="sr-only">Message</span>
        <textarea
          value={text}
          onChange={(event) => setText(event.target.value)}
          onKeyDown={handleKeyDown}
          rows={2}
          placeholder="Ask the assistant..."
          className="w-full rounded border border-gray-300 px-2 py-1 text-sm"
        />
      </label>
      <button
        type="submit"
        disabled={disabled || text.trim().length === 0}
        className="rounded bg-gray-900 px-4 py-2 text-sm text-white disabled:opacity-50"
      >
        Send
      </button>
    </form>
  );
}
