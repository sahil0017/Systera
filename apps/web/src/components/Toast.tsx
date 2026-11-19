export interface ToastMessage {
  id: number;
  kind: 'info' | 'success' | 'error';
  text: string;
}

interface ToastStackProps {
  toasts: ToastMessage[];
  onDismiss: (id: number) => void;
}

export function ToastStack({ toasts, onDismiss }: ToastStackProps) {
  if (toasts.length === 0) return null;
  return (
    <div className="toast-stack" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={`toast toast-${t.kind}`} onClick={() => onDismiss(t.id)}>
          <span className="toast-dot" />
          <span>{t.text}</span>
        </div>
      ))}
    </div>
  );
}
