import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class Config extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

Config.init(
    {
        name: DataTypes.STRING,
        value: DataTypes.STRING,
    },
    {
        sequelize,
        modelName: "Config",
    }
);

export default Config;
