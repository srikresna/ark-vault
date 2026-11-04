# DESIGN.md

Arah visual ARK Vault web.

## Identity

Alat kerja, bukan halaman pemasaran. Terasa seperti instrumen: rapat, tenang,
padat data, tanpa hiasan. Anggota komunitas membukanya untuk mencatat trade dan
membaca angkanya, lalu pergi.

## Personality

- Presisi, bukan ramah berlebihan.
- Percaya diri lewat kerapian, bukan lewat dekorasi.
- Serius tentang angka: tipografi harus membuat angka mudah dibandingkan.

## Palet

Monokrom murni. Tidak ada warna aksen. Ini keputusan pemilik, bukan kelalaian.

| Token | Nilai | Pakai untuk |
|---|---|---|
| `--ink` | `#0a0a0a` | teks utama, tombol primer |
| `--ink-2` | `#3f3f3f` | teks sekunder |
| `--ink-3` | `#6b6b6b` | label, metadata |
| `--line` | `#d8d8d8` | batas, garis tabel |
| `--line-2` | `#ececec` | batas halus, pemisah baris |
| `--surface` | `#ffffff` | latar utama |
| `--surface-2` | `#f6f6f6` | latar panel, header tabel |
| `--surface-3` | `#eeeeee` | hover, baris terpilih |

Karena tidak ada warna, hierarki dibangun dari tiga hal: **berat huruf**,
**ukuran**, dan **ruang**. Angka hasil (WIN, LOSS, RR) dibedakan dengan berat dan
tanda (`+`/`−`), bukan warna. Ini juga membuat tampilan aman untuk buta warna.

## Tipografi

Satu keluarga sans untuk UI, satu keluarga monospace untuk angka.

- UI: system sans stack (`ui-sans-serif`, `Segoe UI`, `Inter` bila tersedia).
  Dipilih karena tidak perlu unduhan dan tetap tajam di ukuran kecil.
- Angka: `ui-monospace`, `SFMono-Regular`, `Menlo`, `Consolas`. Monospace dipakai
  **hanya** untuk nilai numerik (RR, win rate, XP) supaya kolom angka rata dan
  mudah dibandingkan. Bukan untuk gaya terminal.

Skala: 12 / 13 / 15 / 18 / 24 / 32 px. Tidak ada ukuran di antaranya.

## Bentuk

- Radius 4px untuk kontrol, 8px untuk panel. Tidak ada pill.
- Garis 1px sebagai pemisah utama, bukan bayangan.
- Bayangan hanya pada dialog (satu elevasi di seluruh aplikasi).
- Tanpa gradien, tanpa glow, tanpa blur.

## Dial

ENERGY 1 / RHYTHM 2 / MOTION 1.

- **ENERGY 1**: tenang. Tidak ada hero, tidak ada ajakan besar. Halaman dibuka
  langsung ke data.
- **RHYTHM 2**: pola konsisten dengan satu penyimpangan per halaman, yaitu blok
  ringkasan di atas yang lebih lebar dari tabel di bawahnya.
- **MOTION 1**: hanya transisi hover dan focus. Tanpa scroll reveal, tanpa
  parallax, tanpa animasi masuk.

## Motif identitas

Garis horizontal tipis yang memisahkan setiap baris tabel dan setiap bagian, dan
label kecil huruf kapital berjarak lebar di atas setiap blok data. Motif ini
diulang konsisten supaya halaman terasa satu sistem.

## Aturan tata letak

- Satu fokus per layar: blok ringkasan di atas, tabel sebagai isi utama.
- Whitespace memisahkan kelompok, bukan mengisi ruang kosong.
- Tabel selalu punya header tetap dan kolom angka rata kanan.
- Lebar konten maksimum 1100px, satu kolom. Tidak ada sidebar permanen; navigasi
  berada di bar atas.
- Di layar sempit, tabel menjadi daftar kartu. Tidak ada gulir horizontal.

## Status kosong, muat, dan galat

Ketiganya wajib ada dan ditulis, bukan sekadar spinner.

- Kosong: satu kalimat yang menjelaskan apa yang belum ada dan satu tindakan.
- Muat: teks "Memuat" dengan indikator garis, tanpa skeleton palsu.
- Galat: kalimat yang menyebut apa yang gagal dan tombol coba lagi.

## Nada tulisan

- Bahasa Indonesia, kalimat pendek, kata kerja konkret.
- Label tombol menyebut tindakan: "Catat trade", bukan "Submit".
- Tidak ada kata pemasaran: seamless, powerful, revolusioner, AI powered.
- Angka selalu dengan satuan: "2.4R", "68%", "12 trade".
