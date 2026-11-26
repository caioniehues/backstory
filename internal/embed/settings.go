package embed

// Device-local settings keys (never synced; settings are human-only). The
// switch is on when SettingRuntime and SettingModel are both set.
const (
	SettingRuntime = "embed.runtime"
	SettingModel   = "embed.model"
	// SettingDevice holds the stable match (a device name, a PCI address or
	// DeviceCPU), never a backend index; it is resolved again at each use.
	SettingDevice = "embed.device"
	// SettingModelHash caches the model file's sha256 as
	// "<size> <mtime ns> <sha256> <path>".
	SettingModelHash = "embed.model_sha256"
)

// Settings is the part of the store embed reads and writes; *store.Store
// has it.
type Settings interface {
	GetSetting(key string) (string, bool, error)
	SetSetting(key, value string) error
}

// LoadSetup reads the saved Setup and whether embeddings are on. They are
// off by default.
func LoadSetup(s Settings) (Setup, bool, error) {
	var out Setup
	for key, dst := range map[string]*string{SettingRuntime: &out.Runtime, SettingModel: &out.Model, SettingDevice: &out.Device} {
		v, _, err := s.GetSetting(key)
		if err != nil {
			return Setup{}, false, err
		}
		*dst = v
	}
	return out, out.Runtime != "" && out.Model != "", nil
}

// SaveSetup turns embeddings on with setup.
func SaveSetup(s Settings, setup Setup) error {
	for _, kv := range [][2]string{{SettingRuntime, setup.Runtime}, {SettingModel, setup.Model}, {SettingDevice, setup.Device}} {
		if err := s.SetSetting(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// TurnOff clears the switch. Vectors are left in place, and the model hash
// cache stays for the next --on.
func TurnOff(s Settings) error {
	return SaveSetup(s, Setup{})
}
