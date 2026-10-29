# Fotos

Acervo de fotos pessoal e leve para o homelab: um único programa em Go com SQLite, sem banco externo, que usa uns 30 a 50 MB de RAM. Uma alternativa bem mais simples ao Immich ou ao Google Fotos para guardar as fotos da família no seu próprio servidor.

> **Sem login.** A app não tem usuário nem senha: quem acessa a porta dela vê, envia e apaga fotos. Deixe ela só na rede de casa (ou atrás de uma VPN como Tailscale/WireGuard). Não abra a porta no roteador.

- **Upload manual pelo navegador** (botão "Enviar" ou arrastar e soltar; só fotos e vídeos, documentos como PDF e Word são recusados), funciona no celular e no PC.
- **Sem duplicatas:** cada arquivo recebe um hash SHA-256. Se você enviar uma foto idêntica a uma que já está no acervo, ela não é salva e aparece um aviso na tela.
- **Aba Recentes:** navegação por **dia em que a foto foi tirada**, não pelo dia do upload. A data vem, nesta ordem, de:
  1. metadados do arquivo: EXIF da foto (`DateTimeOriginal`, em JPEG, HEIC, PNG e WebP) ou data de gravação do vídeo (MOV/MP4, incluindo a do iPhone);
  2. data no nome do arquivo (`IMG_20240512_101500.jpg`, `IMG-20240512-WA0001.jpg`, `PXL_...`, `2024-05-12 10.15.00.jpg`);
  3. data de modificação do arquivo;
  4. dia do upload, se nada acima existir.
- **Favoritos:** toque na ☆ no visualizador (ou atalho `f`), ou selecione várias fotos e use "Favoritar". Todas aparecem na aba **Favoritos**.
- **Álbuns:** crie álbuns na aba **Álbuns** e adicione fotos pelo botão "Álbuns" do visualizador ou selecionando várias de uma vez. Uma foto pode estar em vários álbuns. Apagar um álbum não apaga as fotos.
- **Lixeira:** apagar uma foto (sozinha ou várias selecionadas) tira ela de Recentes, Favoritos e álbuns e manda para a aba **Lixeira**. Lá dá para restaurar (ela volta aos álbuns e aos favoritos), excluir de vez ou esvaziar tudo. Cada foto é excluída de vez sozinha 30 dias depois de ir para a lixeira (a verificação roda ao iniciar e de hora em hora). Excluir de vez apaga o arquivo original, a miniatura e o registro no banco.
- Miniaturas geradas no upload para JPEG, PNG, GIF, WebP e vídeos (um quadro do vídeo, via ffmpeg, que já vem na imagem Docker). Vídeos aparecem com um ícone de play na miniatura, e as contagens separam fotos de vídeos. Ao iniciar, a app gera em segundo plano as miniaturas que faltam dos vídeos antigos. HEIC é guardado e aberto normalmente, mas aparece sem miniatura.

## Instalação

### Do que você precisa

- Um servidor Linux com **Docker** e o **Docker Compose** ([como instalar](https://docs.docker.com/engine/install/)). Funciona em máquina fraca: a app fica limitada a 512 MB de RAM e 1,5 CPU.
- Espaço em disco para as fotos.

### Subindo

```sh
git clone https://github.com/LucasZansavioPereira/fotos.git
cd fotos
docker compose up -d --build
```

A primeira subida demora uns minutos, porque compila a app. Depois é só abrir `http://IP-DO-SERVIDOR:8085` em qualquer aparelho da rede de casa (o IP aparece com `hostname -I`).

### Configuração

Tudo é opcional. Crie um arquivo `.env` ao lado do `docker-compose.yml`:

```sh
# porta no servidor
FOTOS_PORT=8085
# fuso horário
TZ=America/Sao_Paulo
```

e rode `docker compose up -d` de novo.

Por padrão as fotos ficam em `./data`, dentro da pasta do projeto. Para guardar em outro disco, troque a linha `- ./data:/data` do `docker-compose.yml` pelo caminho dele (ex.: `- /mnt/hd/fotos:/data`).

## Onde ficam as coisas

```
data/
  fotos.db                 banco SQLite (índice, hashes, datas, álbuns)
  photos/AAAA/MM/DD/...    arquivos originais, intactos
  thumbs/...               miniaturas
```

Os originais nunca são alterados, então mesmo sem a app as fotos continuam organizadas em pastas por dia.

**Backup:** guarde a pasta `data/` inteira. Se perder só o `fotos.db`, as fotos continuam lá, e dá para trazer de volta com o comando de importar abaixo (álbuns e favoritos se perdem).

## Atualizar

```sh
git pull
docker compose up -d --build
```

A pasta `data/` é mantida.

## Importar uma pasta de uma vez

Para trazer muitas fotos que já estão no servidor (com a mesma regra de duplicatas):

```sh
docker compose run --rm -v /caminho/das/fotos:/import:ro fotos import /import
```

A pasta é lida só para leitura (`:ro`): os arquivos de lá não são movidos nem apagados.

```sh
# exemplo: trazer um HD externo montado em /media/hd/Fotos
docker compose run --rm -v /media/hd/Fotos:/import:ro fotos import /import
```

## Corrigir datas de arquivos já enviados

Fotos e vídeos enviados antes de a app entender os metadados deles podem ter ficado no dia errado. Para reler a data de todos que não tinham data nos metadados e mover para o dia certo:

```sh
docker compose exec fotos fotos redate
```

## Desenvolvimento

```sh
go run .            # abre em http://localhost:8080, dados em ./data
```

Variáveis: `FOTOS_DATA` (pasta de dados, padrão `./data`) e `FOTOS_ADDR` (endereço, padrão `:8080`).

## Licença

MIT. Veja [LICENSE](LICENSE).
