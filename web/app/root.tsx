import { Links, Meta, Outlet, Scripts, ScrollRestoration, isRouteErrorResponse, useRouteError, Link } from "react-router";
import { Theme } from "@astryxdesign/core/theme";
import { neutralTheme } from "@astryxdesign/theme-neutral/built";
import "@astryxdesign/core/reset.css";
import "@astryxdesign/core/astryx.css";
import "@astryxdesign/theme-neutral/fonts.css";
import "@astryxdesign/theme-neutral/theme.css";
import "./app.css";
import { SessionProvider } from "./lib/session";
export function Layout({ children }: {
    children: React.ReactNode;
}) { return <html lang="en"><head><meta charSet="utf-8"/><meta name="viewport" content="width=device-width, initial-scale=1"/><link rel="icon" type="image/x-icon" href="/brand/favicon.ico" sizes="16x16 32x32 48x48"/><link rel="icon" type="image/svg+xml" href="/brand/favicon.svg" sizes="any"/><link rel="apple-touch-icon" href="/brand/apple-touch-icon.png"/><Meta /><Links /></head><body>{children}<ScrollRestoration /><Scripts /></body></html>; }
export default function App() { return <Theme theme={neutralTheme} mode="light"><SessionProvider><Outlet /></SessionProvider></Theme>; }

export function ErrorBoundary() {
 const error=useRouteError();const missing=isRouteErrorResponse(error)&&error.status===404;
 return <main className="standalone"><h1>{missing?'Page unavailable':'Unable to open this page'}</h1><p>{missing?'This page could not be found.':'Refresh the page and try again.'}</p><Link to="/organizations">Return to your workspace</Link></main>;
}
