import { createContext, useContext, useState, type ReactNode } from 'react';
import { Theme } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';
const Appearance = createContext({ mode: 'light', toggle: () => {} });
export function AppearanceProvider({ children }: { children: ReactNode }) {
    const [mode, setMode] = useState<'light' | 'dark'>('light');
    return <Appearance.Provider value={{ mode, toggle: () => setMode(v => v === 'light' ? 'dark' : 'light') }}><Theme theme={neutralTheme} mode={mode}><div className="app-surface" data-appearance={mode}>{children}</div></Theme></Appearance.Provider>;
}
export function useAppearance() { return useContext(Appearance); }
