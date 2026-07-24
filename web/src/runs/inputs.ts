/**
 * parseInputs turns key=value lines into the inputs map the gateway's
 * RunWorkflow body expects. Blank lines and lines without an `=` are ignored,
 * so a stray comment or trailing newline never becomes a bogus input. Shared
 * by the run monitor and the builder's run panel.
 */
export function parseInputs(text: string): Record<string, string> {
  const inputs: Record<string, string> = {};
  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    const eq = trimmed.indexOf('=');
    if (eq <= 0) {
      continue;
    }
    inputs[trimmed.slice(0, eq).trim()] = trimmed.slice(eq + 1).trim();
  }
  return inputs;
}
