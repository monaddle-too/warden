import {
  networkLabel,
  networkModes,
  networkText,
  type InstallNetwork,
  type NetworkMode,
} from "../network";

// One select for a workspace's network access — follow the install,
// restricted, or open — with a line saying what the choice means.
export function NetworkSelect({
  value,
  install,
  onChange,
  disabled,
}: {
  value: NetworkMode;
  install?: InstallNetwork;
  onChange: (mode: NetworkMode) => void;
  disabled?: boolean;
}) {
  return (
    <>
      <label>
        <select
          aria-label="Network access"
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value as NetworkMode)}
        >
          {networkModes.map((mode) => (
            <option key={mode} value={mode}>
              {networkLabel(mode, install)}
            </option>
          ))}
        </select>
      </label>
      <p className="muted">{networkText(value, install)}</p>
    </>
  );
}
