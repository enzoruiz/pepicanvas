import {
  type FormEvent,
  type ReactNode,
  useEffect,
  useRef,
  useState,
} from "react";

import {
  type ApiError,
  getSession,
  register,
  type Session,
  signIn,
  signOut,
} from "./api";

// Safe, fixed messages: no server text, token, or other user's data is shown.
const messages: Record<ApiError, string> = {
  bad_request: "Enter a valid email and a password of 12 to 128 characters.",
  unauthorized: "Email or password is incorrect.",
  forbidden: "The request was rejected. Reload the page and try again.",
  not_found: "The service is unavailable. Try again.",
  conflict: "The request changed while it was being retried. Try again.",
  identity_taken: "An account with this email already exists.",
  rate_limited: "Too many attempts. Wait a few minutes and try again.",
  unavailable: "The service is unavailable. Try again.",
};

type Mode = "sign-in" | "register";

interface PendingRegistration {
  payload: string;
  operationId: string;
}

interface AuthGateProps {
  children: (session: Session) => ReactNode;
  newOperationId?: () => string;
}

export function AuthGate({
  children,
  newOperationId = () => crypto.randomUUID(),
}: AuthGateProps) {
  const [session, setSession] = useState<Session | null>(null);
  const [checking, setChecking] = useState(true);
  const [mode, setMode] = useState<Mode>("sign-in");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // A registration keeps its operation ID until it gets a definitive answer,
  // so a retry after a network failure cannot create a second account.
  const pending = useRef<PendingRegistration | null>(null);

  useEffect(() => {
    let active = true;
    void getSession().then((result) => {
      if (!active) return;
      setSession(result.ok ? result.value : null);
      setChecking(false);
    });
    return () => {
      active = false;
    };
  }, []);

  function switchMode(next: Mode) {
    setMode(next);
    setError(null);
    setNotice(null);
    setPassword("");
  }

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      if (mode === "sign-in") {
        const result = await signIn(email, password);
        // Clear the password after every attempt, so it never stays in the DOM.
        setPassword("");
        if (result.ok) {
          setSession(result.value);
        } else {
          setError(messages[result.error]);
        }
        return;
      }
      const payload = JSON.stringify([email, password]);
      if (pending.current?.payload !== payload) {
        pending.current = { payload, operationId: newOperationId() };
      }
      const result = await register(
        pending.current.operationId,
        email,
        password,
      );
      // Without an answer the same payload is retried under the same
      // operation ID; any answer ends the operation and clears the password.
      if (result.ok || result.error !== "unavailable") {
        pending.current = null;
        setPassword("");
      }
      if (result.ok) {
        switchMode("sign-in");
        setNotice("Account created. You can sign in now.");
      } else {
        setError(messages[result.error]);
      }
    } finally {
      setBusy(false);
    }
  }

  async function leave() {
    if (!session) return;
    setBusy(true);
    const result = await signOut(session.csrfToken);
    setBusy(false);
    if (result.ok) {
      setSession(null);
      switchMode("sign-in");
    } else {
      setError(messages[result.error]);
    }
  }

  if (checking) {
    return <p role="status">Checking your session…</p>;
  }

  if (session) {
    return (
      <section aria-label="Account">
        <p>
          Signed in as <strong>{session.email}</strong>
        </p>
        <button type="button" onClick={() => void leave()} disabled={busy}>
          Sign out
        </button>
        {error && <p role="alert">{error}</p>}
        {children(session)}
      </section>
    );
  }

  const registering = mode === "register";
  return (
    <section aria-label={registering ? "Create account" : "Sign in"}>
      <h2>{registering ? "Create account" : "Sign in"}</h2>
      {notice && <p role="status">{notice}</p>}
      <form onSubmit={(event) => void submit(event)} noValidate>
        <label>
          Email
          <input
            type="email"
            autoComplete="email"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            required
          />
        </label>
        <label>
          Password
          <input
            type="password"
            autoComplete={registering ? "new-password" : "current-password"}
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            minLength={12}
            maxLength={128}
            required
          />
        </label>
        <button type="submit" disabled={busy}>
          {registering ? "Create account" : "Sign in"}
        </button>
      </form>
      {error && <p role="alert">{error}</p>}
      <button
        type="button"
        onClick={() => switchMode(registering ? "sign-in" : "register")}
      >
        {registering ? "I already have an account" : "Create an account"}
      </button>
    </section>
  );
}
