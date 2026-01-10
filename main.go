package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"math"
	"sort"
	"gopkg.in/yaml.v3"
)

type Config struct {
    IIOPath     string `yaml:"iio_path"`
    Name        string `yaml:"name"`
    Addr        string `yaml:"addr"`
    Rate        int    `yaml:"rate"`
    LogEvery    int    `yaml:"log_every"`
    SetScales   *bool  `yaml:"set_scales"`
    SetRate     *bool  `yaml:"set_rate"`
    MountMatrix struct {
        X []float64 `yaml:"x"`
        Y []float64 `yaml:"y"`
        Z []float64 `yaml:"z"`
    } `yaml:"mount_matrix"`
}

func loadConfigFile() (*Config, error) {
    cfgPath := filepath.Join(os.Getenv("HOME"), ".config", "iio-dsu-bridge.yaml")
    b, err := os.ReadFile(cfgPath)
    if err != nil {
        return &Config{}, nil // silencioso si no existe
    }
    var c Config
    if err := yaml.Unmarshal(b, &c); err != nil {
        return nil, err
    }
    return &c, nil
}

type Vec3 struct{ X, Y, Z float64 }

type IMUSample struct {
	Gyro  Vec3  // rad/s
	Accel Vec3  // m/s^2
	TSus  uint64
}

type MountMatrix struct {
	X Vec3
	Y Vec3
	Z Vec3
}

func (m MountMatrix) Apply(v Vec3) Vec3 {
	return Vec3{
		X: m.X.X*v.X + m.X.Y*v.Y + m.X.Z*v.Z,
		Y: m.Y.X*v.X + m.Y.Y*v.Y + m.Y.Z*v.Z,
		Z: m.Z.X*v.X + m.Z.Y*v.Y + m.Z.Z*v.Z,
	}
}

// ---------- IIO helpers ----------

func findIIODeviceByName(name string) (string, error) {
	base := "/sys/bus/iio/devices"
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	nameLower := strings.ToLower(name)

	var exact, partial, firstWithIMU string

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "iio:device") {
			continue
		}
		dev := filepath.Join(base, e.Name())
		b, _ := os.ReadFile(filepath.Join(dev, "name"))
		devName := strings.TrimSpace(string(b))
		devLower := strings.ToLower(devName)

		hasGyro := fileExists(filepath.Join(dev, "in_anglvel_x_raw"))
		hasAccel := fileExists(filepath.Join(dev, "in_accel_x_raw"))
		if firstWithIMU == "" && (hasGyro || hasAccel) {
			firstWithIMU = dev
		}
		// si no se pidió nombre, devolvemos el primero con IMU
		if nameLower == "" {
			if firstWithIMU != "" {
				return firstWithIMU, nil
			}
			continue
		}
		// match exacto (case-insensitive)
		if devLower == nameLower {
			exact = dev
		}
		// match parcial
		if strings.Contains(devLower, nameLower) || strings.Contains(nameLower, devLower) {
			if partial == "" {
				partial = dev
			}
		}
	}
	switch {
	case exact != "":
		return exact, nil
	case partial != "":
		return partial, nil
	case firstWithIMU != "":
		return firstWithIMU, nil
	default:
		return "", fmt.Errorf("iio device with name %q not found", name)
	}
}

func findFirstIIODeviceWith(wantGyro, wantAccel bool) (string, error) {
	if !wantGyro && !wantAccel {
		return "", fmt.Errorf("must request gyro and/or accel")
	}
	base := "/sys/bus/iio/devices"
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "iio:device") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, n := range names {
		dev := filepath.Join(base, n)
		hasGyro := fileExists(filepath.Join(dev, "in_anglvel_x_raw"))
		hasAccel := fileExists(filepath.Join(dev, "in_accel_x_raw"))
		if (wantGyro && !hasGyro) || (wantAccel && !hasAccel) {
			continue
		}
		return dev, nil
	}
	return "", fmt.Errorf("no matching IIO device found (gyro=%v accel=%v)", wantGyro, wantAccel)
}

func readFloat(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	// soporta notación científica
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("parseFloat %q: %w", s, err)
	}
	return f, nil
}

func readInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	// algunos kernels dejan un \n extra; otros tifilan con espacios
	s = strings.Fields(s)[0]
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parseInt %q: %w", s, err)
	}
	return v, nil
}

func readFloatIfExists(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil { return 0, false }
	s := strings.TrimSpace(string(b))
	f, err := strconv.ParseFloat(s, 64)
	if err != nil { return 0, false }
	return f, true
}

func readFloatList(path string) ([]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil { return nil, err }
	fields := strings.Fields(string(b))
	out := make([]float64, 0, len(fields))
	for _, f := range fields {
		v, err := strconv.ParseFloat(f, 64)
		if err != nil { continue }
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no floats in %s", path)
	}
	return out, nil
}

func writeFloat(path string, v float64) error {
	s := fmt.Sprintf("%.9g", v) // formato compact
	return os.WriteFile(path, []byte(s), 0644)
}

func writeInt(path string, v int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(v)), 0644)
}

func nearest(avail []float64, target float64) float64 {
	if len(avail) == 0 { return target }
	best := avail[0]
	minDiff := math.Abs(avail[0]-target)
	for _, a := range avail[1:] {
		if d := math.Abs(a-target); d < minDiff {
			minDiff = d; best = a
		}
	}
	return best
}

func listIIODevices() {
	base := "/sys/bus/iio/devices"
	entries, err := os.ReadDir(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", base, err)
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "iio:device") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		dev := filepath.Join(base, n)
		nameBytes, _ := os.ReadFile(filepath.Join(dev, "name"))
		name := strings.TrimSpace(string(nameBytes))
		hasGyro := fileExists(filepath.Join(dev, "in_anglvel_x_raw"))
		hasAccel := fileExists(filepath.Join(dev, "in_accel_x_raw"))
		gScale, _ := readFloatIfExists(filepath.Join(dev, "in_anglvel_scale"))
		aScale, _ := readFloatIfExists(filepath.Join(dev, "in_accel_scale"))
		fmt.Printf("%s  name=%q  gyro=%v accel=%v  gScale=%g aScale=%g\n", dev, name, hasGyro, hasAccel, gScale, aScale)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

type IIODevice struct {
	Base          string
	GyroScale     Vec3
	AccelScale    Vec3
	HaveAccel     bool
	HaveGyro      bool
	AngVelPaths   [3]string
	AccelPaths    [3]string
	AngVelScaleP  [3]string
	AccelScaleP   [3]string
	SampleRateHz  float64
	AccelRateHz   float64
	AngVelRateHz  float64
}

func openIIODevice(base string) (*IIODevice, error) {
	dev := &IIODevice{Base: base}

	// canales raw
	dev.AngVelPaths[0] = filepath.Join(base, "in_anglvel_x_raw")
	dev.AngVelPaths[1] = filepath.Join(base, "in_anglvel_y_raw")
	dev.AngVelPaths[2] = filepath.Join(base, "in_anglvel_z_raw")
	dev.AccelPaths[0] = filepath.Join(base, "in_accel_x_raw")
	dev.AccelPaths[1] = filepath.Join(base, "in_accel_y_raw")
	dev.AccelPaths[2] = filepath.Join(base, "in_accel_z_raw")

	// escalas
	dev.AngVelScaleP[0] = filepath.Join(base, "in_anglvel_x_scale")
	dev.AngVelScaleP[1] = filepath.Join(base, "in_anglvel_y_scale")
	dev.AngVelScaleP[2] = filepath.Join(base, "in_anglvel_z_scale")
	dev.AccelScaleP[0] = filepath.Join(base, "in_accel_x_scale")
	dev.AccelScaleP[1] = filepath.Join(base, "in_accel_y_scale")
	dev.AccelScaleP[2] = filepath.Join(base, "in_accel_z_scale")

	// detectar presencia
	if _, err := os.Stat(dev.AngVelPaths[0]); err == nil {
		dev.HaveGyro = true
	}
	if _, err := os.Stat(dev.AccelPaths[0]); err == nil {
		dev.HaveAccel = true
	}
	if !dev.HaveGyro && !dev.HaveAccel {
		return nil, errors.New("no gyro/accel channels found in IIO device")
	}

	// leer escalas (si falta o da 0, intentar global)
	if dev.HaveGyro {
		var sx, sy, sz float64
		if v, ok := readFloatIfExists(dev.AngVelScaleP[0]); ok { sx = v }
		if v, ok := readFloatIfExists(dev.AngVelScaleP[1]); ok { sy = v } else { sy = sx }
		if v, ok := readFloatIfExists(dev.AngVelScaleP[2]); ok { sz = v } else { sz = sx }
		// fallback global
		if sx == 0 && sy == 0 && sz == 0 {
			if v, ok := readFloatIfExists(filepath.Join(base, "in_anglvel_scale")); ok {
				sx, sy, sz = v, v, v
			}
		}
		dev.GyroScale = Vec3{X: sx, Y: sy, Z: sz}
	}
	if dev.HaveAccel {
		var sx, sy, sz float64
		if v, ok := readFloatIfExists(dev.AccelScaleP[0]); ok { sx = v }
		if v, ok := readFloatIfExists(dev.AccelScaleP[1]); ok { sy = v } else { sy = sx }
		if v, ok := readFloatIfExists(dev.AccelScaleP[2]); ok { sz = v } else { sz = sx }
		// fallback global
		if sx == 0 && sy == 0 && sz == 0 {
			if v, ok := readFloatIfExists(filepath.Join(base, "in_accel_scale")); ok {
				sx, sy, sz = v, v, v
			}
		}
		dev.AccelScale = Vec3{X: sx, Y: sy, Z: sz}
	}


	// sample rates (si existen)
	if f, err := readFloat(filepath.Join(base, "in_anglvel_sampling_frequency")); err == nil {
		dev.AngVelRateHz = f
	}
	if f, err := readFloat(filepath.Join(base, "in_accel_sampling_frequency")); err == nil {
		dev.AccelRateHz = f
	}

	return dev, nil
}

func (d *IIODevice) readSample() (IMUSample, error) {
	s := IMUSample{TSus: uint64(time.Now().UnixMicro())}
	if d.HaveGyro {
		rx, err := readInt(d.AngVelPaths[0]); if err != nil { return s, err }
		ry, err := readInt(d.AngVelPaths[1]); if err != nil { return s, err }
		rz, err := readInt(d.AngVelPaths[2]); if err != nil { return s, err }
		// convertir a rad/s (IIO suministra en unidades del sensor: raw * scale = rad/s)
		s.Gyro = Vec3{
			X: float64(rx) * d.GyroScale.X,
			Y: float64(ry) * d.GyroScale.Y,
			Z: float64(rz) * d.GyroScale.Z,
		}
	}
	if d.HaveAccel {
		ax, err := readInt(d.AccelPaths[0]); if err != nil { return s, err }
		ay, err := readInt(d.AccelPaths[1]); if err != nil { return s, err }
		az, err := readInt(d.AccelPaths[2]); if err != nil { return s, err }
		// convertir a m/s^2 (raw * scale = m/s^2)
		s.Accel = Vec3{
			X: float64(ax) * d.AccelScale.X,
			Y: float64(ay) * d.AccelScale.Y,
			Z: float64(az) * d.AccelScale.Z,
		}
	}
	return s, nil
}

// ---------- DSU packet builders (PLACEHOLDER: pegar serializer conocido) ----------

// buildControllerInfo debe devolver un paquete DSU "ControllerInfo" válido.
// Recomendación fuerte: copiar aquí la construcción exacta de SteamDeckGyroDSU
// (o de otro server DSU confiable) para garantizar compatibilidad con Yuzu.
func buildControllerInfo() []byte {
	// *** PLACEHOLDER ***: No improvisar el binario DSU.
	// Devolvemos algo vacío para que compile; Yuzu no lo aceptará así.
	return []byte{}
}

// buildControllerData idem: pegar implementación correcta (orden de campos, endian, etc.)
func buildControllerData(s IMUSample) []byte {
	// *** PLACEHOLDER ***
	_ = binary.LittleEndian
	_ = bytes.NewBuffer(nil)
	return []byte{}
}

// ---------- Main ----------

func main() {
	name := flag.String("name", "", "IIO device name (from /sys/bus/iio/devices/iio:deviceX/name, empty=auto)")
	iioPath := flag.String("iio-path", "", "Explicit /sys/bus/iio/devices/iio:deviceX path (overrides --name)")
	listIIO := flag.Bool("list-iio", false, "List detected IIO devices and exit")
	addr := flag.String("addr", "127.0.0.1:26760", "DSU UDP destination")
	rate := flag.Int("rate", 250, "Output rate (Hz)")
	logEvery := flag.Int("log-every", 25, "Print one IMU line every N samples (0=off)")
	setScales := flag.Bool("set-scales", true, "If scales read as 0, set them to a valid value automatically")
	setRate := flag.Bool("set-rate", true, "Try to set sampling_frequency close to --rate")
	flag.Parse()

	if *listIIO {
		listIIODevices()
		os.Exit(0)
	}

	cfg, _ := loadConfigFile()

	// ENV override
	if v := os.Getenv("IIO_DSU_PATH"); v != "" { cfg.IIOPath = v }
	if v := os.Getenv("IIO_DSU_NAME"); v != "" { cfg.Name = v }
	if v := os.Getenv("IIO_DSU_ADDR"); v != "" { cfg.Addr = v }
	if v := os.Getenv("IIO_DSU_RATE"); v != "" { if iv,err := strconv.Atoi(v); err==nil { cfg.Rate = iv } }
	if v := os.Getenv("IIO_DSU_LOG_EVERY"); v != "" { if iv,err := strconv.Atoi(v); err==nil { cfg.LogEvery = iv } }
	if v := os.Getenv("IIO_DSU_SET_SCALES"); v != "" { b := v=="1" || strings.ToLower(v)=="true"; cfg.SetScales = &b }
	if v := os.Getenv("IIO_DSU_SET_RATE"); v != "" { b := v=="1" || strings.ToLower(v)=="true"; cfg.SetRate = &b }

	// Flags ganan sobre todo
	if *iioPath != "" { cfg.IIOPath = *iioPath }
	if *name != "" { cfg.Name = *name }  // solo si el flag trae algo
	if *addr != "" { cfg.Addr = *addr }
	if *rate != 0 { cfg.Rate = *rate }
	if *logEvery >= 0 { cfg.LogEvery = *logEvery }
	if cfg.SetScales == nil { cfg.SetScales = setScales } else { *setScales = *cfg.SetScales }
	if cfg.SetRate == nil   { cfg.SetRate   = setRate }   else { *setRate   = *cfg.SetRate }

	if cfg.Addr == "" { cfg.Addr = "127.0.0.1:26760" }
	if cfg.Rate == 0  { cfg.Rate = 250 }

	// Elegir device
	var iioBase string
	var err error
	if cfg.IIOPath != "" {
		iioBase = cfg.IIOPath
	} else {
		iioBase, err = findIIODeviceByName(cfg.Name)
		if err != nil {
			// fallback duro si existe iio:device0
			if fileExists("/sys/bus/iio/devices/iio:device0") {
				iioBase = "/sys/bus/iio/devices/iio:device0"
				fmt.Fprintf(os.Stderr, "WARN: name=%q not found; falling back to %s\n", cfg.Name, iioBase)
			} else {
				fmt.Fprintf(os.Stderr, "IIO device not found (name=%q). Tip: try --list-iio or --iio-path=/sys/bus/iio/devices/iio:deviceX\n", cfg.Name)
				listIIODevices()
				os.Exit(1)
			}
		}
	}

	dev, err := openIIODevice(iioBase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "openIIODevice: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("IIO base: %s\n", iioBase)
	fmt.Printf("HaveGyro=%v GyroScale=(%.6f,%.6f,%.6f)  HaveAccel=%v AccelScale=(%.6f,%.6f,%.6f)\n",
		dev.HaveGyro, dev.GyroScale.X, dev.GyroScale.Y, dev.GyroScale.Z,
		dev.HaveAccel, dev.AccelScale.X, dev.AccelScale.Y, dev.AccelScale.Z)

	// If the selected IIO device is split (accel-only or gyro-only), try to open the complementary device.
	var gyroDev *IIODevice
	var accelDev *IIODevice
	baseClean := filepath.Clean(dev.Base)

	if dev.HaveGyro && !dev.HaveAccel {
		if p, err := findFirstIIODeviceWith(false, true); err == nil && filepath.Clean(p) != baseClean {
			if d2, err := openIIODevice(p); err == nil && d2.HaveAccel {
				accelDev = d2
				fmt.Printf("Using additional accel device: %s\n", p)
			}
		}
	} else if dev.HaveAccel && !dev.HaveGyro {
		if p, err := findFirstIIODeviceWith(true, false); err == nil && filepath.Clean(p) != baseClean {
			if d2, err := openIIODevice(p); err == nil && d2.HaveGyro {
				gyroDev = d2
				fmt.Printf("Using additional gyro device: %s\n", p)
			}
		}
	}
	
	// Auto-set scales if requested and currently zero
	if *setScales {
		changed := false
		// Gyro
		if dev.HaveGyro && dev.GyroScale.X == 0 && dev.GyroScale.Y == 0 && dev.GyroScale.Z == 0 {
			if avail, err := readFloatList(filepath.Join(iioBase, "in_anglvel_scales_available")); err == nil {
				pick := avail[len(avail)/2] // el del medio
				if err := writeFloat(filepath.Join(iioBase, "in_anglvel_scale"), pick); err == nil {
					fmt.Printf("Set in_anglvel_scale=%g\n", pick)
					dev.GyroScale = Vec3{X: pick, Y: pick, Z: pick}
					changed = true
				}
			}
		}
		// Accel
		if dev.HaveAccel && dev.AccelScale.X == 0 && dev.AccelScale.Y == 0 && dev.AccelScale.Z == 0 {
			if avail, err := readFloatList(filepath.Join(iioBase, "in_accel_scales_available")); err == nil {
				pick := avail[len(avail)/2]
				if err := writeFloat(filepath.Join(iioBase, "in_accel_scale"), pick); err == nil {
					fmt.Printf("Set in_accel_scale=%g\n", pick)
					dev.AccelScale = Vec3{X: pick, Y: pick, Z: pick}
					changed = true
				}
			}
		}
		if changed {
			fmt.Printf("New scales → Gyro(%.6f) Accel(%.6f)\n", dev.GyroScale.X, dev.AccelScale.X)
		}
	}

	// Auto-set sampling frequency
	if *setRate {
		// gyro
		if dev.HaveGyro {
			if avail, err := readFloatList(filepath.Join(iioBase, "in_anglvel_sampling_frequency_available")); err == nil {
				pick := nearest(avail, float64(*rate))
				if err := writeFloat(filepath.Join(iioBase, "in_anglvel_sampling_frequency"), pick); err == nil {
					fmt.Printf("Set in_anglvel_sampling_frequency=%g\n", pick)
				}
			}
		}
		// accel
		if dev.HaveAccel {
			if avail, err := readFloatList(filepath.Join(iioBase, "in_accel_sampling_frequency_available")); err == nil {
				pick := nearest(avail, float64(*rate))
				if err := writeFloat(filepath.Join(iioBase, "in_accel_sampling_frequency"), pick); err == nil {
					fmt.Printf("Set in_accel_sampling_frequency=%g\n", pick)
				}
			}
		}
	}

	// Mount matrix igual a tu YAML:
	// x: [1, 0, 0]
	// y: [0, -1, 0]
	// z: [0, 0, -1]
	mount := MountMatrix{
		X: Vec3{1, 0, 0},
		Y: Vec3{0,-1, 0},
		Z: Vec3{0, 0,-1},
	}
	if len(cfg.MountMatrix.X)==3 && len(cfg.MountMatrix.Y)==3 && len(cfg.MountMatrix.Z)==3 {
		mount = MountMatrix{
			X: Vec3{cfg.MountMatrix.X[0], cfg.MountMatrix.X[1], cfg.MountMatrix.X[2]},
			Y: Vec3{cfg.MountMatrix.Y[0], cfg.MountMatrix.Y[1], cfg.MountMatrix.Y[2]},
			Z: Vec3{cfg.MountMatrix.Z[0], cfg.MountMatrix.Z[1], cfg.MountMatrix.Z[2]},
		}
	}

	// DSU server: escucha en 0.0.0.0:26760 (lo espera Yuzu/Cemuhook)
	srv, err := NewDSUServer("0.0.0.0:26760")
	if err != nil {
		fmt.Fprintf(os.Stderr, "DSU listen: %v\n", err)
		os.Exit(1)
	}
	defer srv.Close()
	fmt.Println("DSU server listening on :26760")

	// Bucle a tasa fija
	ticker := time.NewTicker(time.Second / time.Duration(*rate))
	defer ticker.Stop()

	count := 0
	for range ticker.C {
		s, err := dev.readSample()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				fmt.Fprintf(os.Stderr, "readSample: %v\n", err)
			}
			continue
		}
		// Merge complementary split-device sample.
		if gyroDev != nil {
			if gs, err2 := gyroDev.readSample(); err2 == nil {
				s.Gyro = gs.Gyro
			}
		}
		if accelDev != nil {
			if as, err2 := accelDev.readSample(); err2 == nil {
				s.Accel = as.Accel
			}
		}
		// aplicar mount matrix
		s.Gyro = mount.Apply(s.Gyro)
		s.Accel = mount.Apply(s.Accel)

		if *logEvery > 0 {
			count++
			if count%*logEvery == 0 {
				fmt.Printf("IMU ts=%d  G(rad/s)=(% .5f,% .5f,% .5f)  A(m/s^2)=(% .3f,% .3f,% .3f)\n",
					s.TSus, s.Gyro.X, s.Gyro.Y, s.Gyro.Z, s.Accel.X, s.Accel.Y, s.Accel.Z)
			}
		}

		srv.Broadcast(s)
	}
}


