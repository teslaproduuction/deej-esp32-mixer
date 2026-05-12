import { writable } from 'svelte/store';

export type Calibration = { min: number; max: number };

export type Config = {
  sliderMapping: Record<number, string[]>;
  comPort: string;
  baudRate: number;
  invertSliders: boolean;
  noiseReduction: number;
  calibration: Record<number, Calibration>;
  ledMode: number;
};

export type AudioSession = {
  pid: number;
  name: string;
  volume: number;
  isSystem: boolean;
};

export const values = writable<number[]>([0, 0, 0, 0, 0]);
export const connected = writable<boolean>(false);
export const status = writable<string>('');
export const selectedPort = writable<string>('');
export const ports = writable<string[]>([]);
export const configPath = writable<string>('');
export const cfg = writable<Config | null>(null);
