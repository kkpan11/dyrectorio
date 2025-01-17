import { Injectable } from '@nestjs/common'
import { Identity } from '@ory/kratos-client'
import { ReadStream } from 'fs'
import { ContainerConfigData, ContainerVolumeType } from 'src/domain/container'
import { toPrismaJson } from 'src/domain/utils'
import { CruxNotFoundException } from 'src/exception/crux-exception'
import {
  deploymentStrategyFromJSON,
  exposeStrategyFromJSON,
  networkModeFromJSON,
  restartPolicyFromJSON,
} from 'src/grpc/protobuf/proto/common'
import PrismaService from 'src/services/prisma.service'
import TemplateFileService, { TemplateContainerConfig, TemplateImage } from 'src/services/template.file.service'
import { VERSIONLESS_PROJECT_VERSION_NAME } from 'src/shared/const'
import { v4 as uuid } from 'uuid'
import ImageMapper from '../image/image.mapper'
import { CreateProjectDto, ProjectDto } from '../project/project.dto'
import ProjectService from '../project/project.service'
import RegistryService from '../registry/registry.service'
import { CreateVersionDto } from '../version/version.dto'
import VersionService from '../version/version.service'
import { CreateProjectFromTemplateDto } from './template.dto'

const VERSION_NAME = '1.0.0'

@Injectable()
export default class TemplateService {
  constructor(
    private prisma: PrismaService,
    private projectService: ProjectService,
    private templateFileService: TemplateFileService,
    private registryService: RegistryService,
    private versionService: VersionService,
    private imageMapper: ImageMapper,
  ) {}

  async createProjectFromTemplate(req: CreateProjectFromTemplateDto, identity: Identity): Promise<ProjectDto> {
    const { teamSlug } = req

    const template = await this.templateFileService.getTemplateById(req.id)

    if (template.registries && template.registries.length > 0) {
      const counts = await this.prisma.registry.findMany({
        where: {
          AND: [
            {
              name: {
                in: template.registries.map(it => it.name),
              },
            },
            {
              team: {
                slug: teamSlug,
              },
            },
          ],
        },
      })

      const createRegistries = template.registries
        .filter(it => !counts.find(f => f.name === it.name))
        .map(it =>
          this.registryService.createRegistry(
            teamSlug,
            {
              ...it,
              description: it.description ?? '',
            },
            identity,
          ),
        )
      await Promise.all(createRegistries)
    }

    const createProjectReq: CreateProjectDto = {
      name: req.name,
      description: req.description,
      type: req.type,
    }

    const project = await this.projectService.createProject(teamSlug, createProjectReq, identity)

    await this.createVersion(teamSlug, template.images, project, identity)

    return project
  }

  async getImageStream(id: string): Promise<ReadStream> {
    try {
      return this.templateFileService.getTemplateImageStreamById(id)
    } catch (err) {
      throw new CruxNotFoundException({ message: 'Template image not found.', property: 'template', value: id })
    }
  }

  private idify<T extends object>(object: T): T {
    return { ...object, id: uuid() }
  }

  private mapTemplateConfig(config: TemplateContainerConfig): Omit<ContainerConfigData, 'storageId'> {
    // TODO (polaroi8d): wait with this for the templates rework
    // TODO (@m8vago): validate containerConfigData

    return {
      ...config,
      deploymentStrategy: config.deploymentStatregy
        ? this.imageMapper.deploymentStrategyToDb(
            deploymentStrategyFromJSON(config.deploymentStatregy.toLocaleUpperCase()),
          )
        : 'recreate',
      restartPolicy: config.restartPolicy
        ? this.imageMapper.restartPolicyToDb(restartPolicyFromJSON(config.restartPolicy.toLocaleUpperCase()))
        : 'no',
      networkMode: config.networkMode
        ? this.imageMapper.networkModeToDb(networkModeFromJSON(config.networkMode.toLocaleUpperCase()))
        : 'bridge',
      expose: config.expose
        ? this.imageMapper.exposeStrategyToDb(exposeStrategyFromJSON(config.expose.toLocaleUpperCase()))
        : 'none',
      networks: config.networks ? config.networks.map(it => ({ id: uuid(), key: it })) : [],
      ports: config.ports ? toPrismaJson(config.ports.map(it => this.idify(it))) : [],
      environment: config.environment ? config.environment.map(it => this.idify(it)) : [],
      args: config.args ? config.args.map(it => this.idify(it)) : [],
      volumes: config.volumes
        ? toPrismaJson(
            config.volumes.map(it => ({
              ...this.idify(it),
              type: it.type ? (it.type as ContainerVolumeType) : 'rwo',
            })),
          )
        : [],
      secrets: config.secrets ? config.secrets.map(it => this.idify(it)) : [],
    }
  }

  private async createVersion(
    teamSlug: string,
    templateImages: TemplateImage[],
    project: ProjectDto,
    identity: Identity,
  ): Promise<void> {
    const { id: projectId } = project

    let version =
      project.type === 'versioned'
        ? await this.prisma.version.findFirst({
            where: {
              name: VERSION_NAME,
              projectId: project.id,
            },
          })
        : await this.prisma.version.findFirst({
            where: {
              name: VERSIONLESS_PROJECT_VERSION_NAME,
              projectId: project.id,
            },
          })

    if (version === null) {
      const createReq: CreateVersionDto = {
        name: VERSION_NAME,
        type: 'incremental',
        changelog: null,
      }

      const newVersion = await this.versionService.createVersion(projectId, createReq, identity)
      version = await this.prisma.version.findFirst({
        where: {
          id: newVersion.id,
        },
      })
    }

    const registryLookup = await this.prisma.registry.findMany({
      where: {
        AND: [
          {
            name: {
              in: templateImages
                .map(it => it.registryName)
                .filter((value, index, array) => array.indexOf(value) === index),
            },
          },
          {
            team: {
              slug: teamSlug,
            },
          },
        ],
      },
    })

    const images = templateImages.map((it, index) => {
      const registryId = registryLookup.find(reg => reg.name === it.registryName).id
      const config = this.mapTemplateConfig(it.config)

      return this.prisma.image.create({
        include: {
          config: true,
          registry: true,
        },
        data: {
          createdBy: identity.id,
          name: it.image,
          order: index,
          tag: it.tag,
          version: { connect: { id: version.id } },
          registry: { connect: { id: registryId } },
          config: {
            create: {
              ...config,
              type: 'image',
              updatedAt: undefined,
              updatedBy: identity.id,
              storage: !it.config.storageId
                ? undefined
                : {
                    connect: {
                      id: it.config.storageId,
                    },
                  },
            },
          },
        },
      })
    })

    await this.prisma.$transaction(images)
  }
}
